/**
 * The skill-cuts runner's command line (README.md, "Commands"). Every command that calls a model
 * takes `--config <skill-cuts.json>` (config.ts: the gateway's address), `--key-file <file>` (the
 * gateway key, read inside gateway.ts and never printed, logged or written), and `--out` (where
 * it writes; `behaviour`'s stage commands, `load` and `build-cases` write session text there, so
 * they refuse an `--out` inside a git or jj checkout, exit 2).
 * `behaviour` and `load` read skill texts from directories, cases or tasks from a file, and the
 * key from a file; the repositories a context needs come from `--repos <dir>` (a tree of
 * `<owner>/<name>/<commit>/` directories, context.ts's `localSource`), or else from GitHub
 * through `gh` (context.ts's `ghSource`). A load with `--test-setup` reads its inventory copy
 * even when all its captures are saved.
 *
 *   bun scripts/skill-cuts/cli.ts behaviour <screen|confirm|control|report> --config <file>
 *       --key-file <file> --out <dir> --old <dir> [--new <dir> | --ours <dir>] --cases <file>
 *       [--case <id>] [--planted-line <text>] [--budget-usd <n>] [--concurrency <n>]
 *       [--after-case <command>] [--models <models.yml>] [--omp <binary>] [--repos <dir>]
 *       [--hint-model <model>] [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>]
 *       [--request-log <file>] [--reading-models <file>] [--limits <file>]
 *       [--build-summary <file>]... (report only)
 *       [--again <case id>]... [--confirm-flagged] (both confirm only)
 *       [--allow-runners <digest>[,<digest>...]] [--store-only <file>] (report only)
 *       [--route-id <arm>=<uuid>]... (screen, confirm and control only)
 *   bun scripts/skill-cuts/cli.ts behaviour audit-mixed --config <file> --out <dir> --old <dir>
 *       [--new <dir> | --ours <dir>] --cases <file> [--cache-dir <dir>] [--scratch <dir>]
 *   bun scripts/skill-cuts/cli.ts behaviour continue-mixed --config <file> --key-file <file>
 *       --from <saved out> --out <new dir> --old <dir> [--new <dir> | --ours <dir>] --cases <file>
 *       [--allow-runners <digest>[,<digest>...]] [--only <file>] [--further-rounds <n>]
 *       [--probe <case id>/<arm>/<run number>] [--budget-usd <n>] [--concurrency <n>]
 *       [--cache-dir <dir>] [--scratch <dir>] [--request-log <file>] [--after-case <command>]
 *       [--repos <dir>] [--models <models.yml>] [--omp <binary>]
 *   bun scripts/skill-cuts/cli.ts read --config <file> --key-file <file> --prompt <name>
 *       --in <input.json> --out <answer.json> [--request-log <file>] [--budget-usd <n>]
 *       [--concurrency <n>]
 *   bun scripts/skill-cuts/cli.ts control <map|attack|same-job|sentence> --config <file>
 *       --key-file <file> --in <input.json> --out <result.json> [what to plant, below]
 *       [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
 *   bun scripts/skill-cuts/cli.ts control craft (--remove-section <heading> | --join-lines <a>-<b>)
 *       --copy-line <a>[-<b>] [--copy-kind <present|missing>]... [--copy-rule <id>]...
 *       [--copy-claim <text>] --original-line <a>[-<b>|all] [--original-kind <k>]...
 *       [--original-rule <id>]... [--original-claim <text>] <craft's flags> --out <dir>
 *   bun scripts/skill-cuts/cli.ts craft-lines --list <craft-list.json>
 *   bun scripts/skill-cuts/cli.ts craft --config <file> --key-file <file> --skill <dir>
 *       --style <file> --style-source <text> --craft <craft-list.json> --craft-source <text>
 *       [--nearest <dir>]... [--skill-source <text>] --out <entry.json> [--request-log <file>]
 *       [--budget-usd <n>] [--concurrency <n>]
 *   bun scripts/skill-cuts/cli.ts craft-merge --entry <entry.json> --entry <entry.json>
 *       [--entry <entry.json>]... --out <merged.json>
 *   bun scripts/skill-cuts/cli.ts nearest --config <file> --key-file <file> --skill <dir>
 *       [--skill-source <text>] --candidates <source>=<root>... --out <row.json>
 *       [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
 *   bun scripts/skill-cuts/cli.ts craft-coverage --root <dir> [--source <text>]
 *       [--skip <name>]... --entries <reader-findings.jsonl>
 *   bun scripts/skill-cuts/cli.ts proposals --config <file> --key-file <file> --body <file>
 *       --body-source <text> --section <heading> --list <text>... --pr-diff <prefix>=<file>...
 *       --skills-root <repo>=<dir>... --repo-root <repo>=<dir> --style <file>
 *       --style-source <text> --craft <craft-list.json> --craft-source <text>
 *       --findings <reader-findings.jsonl> --out <dir> [--request-log <file>] [--budget-usd <n>]
 *       [--concurrency <n>]
 *   bun scripts/skill-cuts/cli.ts proposals --dry-run --body <file> --section <heading>
 *       --list <text>...
 *   bun scripts/skill-cuts/cli.ts count-sql --config <file> --store <db> --day <YYYY-MM-DD>
 *       [--hour <0-23>] [--query count|models]
 *   bun scripts/skill-cuts/cli.ts count-sql --config <file> --store <db> --query roots|hashes
 *   bun scripts/skill-cuts/cli.ts count-sql --config <file> --query transcript --day <YYYY-MM-DD>
 *       [--hour <0-23>] [--run]
 *   bun scripts/skill-cuts/cli.ts count-sql --config <file> --query transcript-coverage [--run]
 *   bun scripts/skill-cuts/cli.ts load --config <file> --key-file <file> --skill <dir>
 *       [--name <name>] --tasks <file> --host <omp|claude> [--profile <file>] --out <dir>
 *       [--runs <n>] [--model <model>] [--thinking <level>] [--budget-usd <n>]
 *       [--concurrency <n>] [--request-log <file>] [--repos <dir>] [--cache-dir <dir>]
 *       [--scratch <dir>] [--models <models.yml>] [--omp <binary>] [--hint-model <model>]
 *       [--hint-draw <n>] [--omp-config <file>]... [--claude <binary>]
 *       [--context-session <store>:<path hash>] [--test-setup <owner/repo>@<commit>:<path>]
 *       [--route-seed <text>]
 *   bun scripts/skill-cuts/cli.ts load audit-mixed --out <dir> --skill <dir> [--cache-dir <dir>]
 *   bun scripts/skill-cuts/cli.ts copies --config <file> --at <ISO instant> --out <dir>
 *       [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]
 *   bun scripts/skill-cuts/cli.ts attribute --config <file> --count <dir> --copies <dir>
 *       --spans <file> --out <dir> [--changed <cut or set id>=<file>]... [--models <dir>]
 *   bun scripts/skill-cuts/cli.ts set-count --config <file> --rows <dir> --spans <file>
 *       --out <dir> [--dotfiles-commit <sha>] [--rows-label <text>] [--not-read <name>=<reason>]...
 *   bun scripts/skill-cuts/cli.ts build-cases --config <file> --key-file <file>
 *       (--cut <id> | --set <id>) --skill <name> --old <dir> [--new <dir>] --sessions <file>
 *       --seed <n> --copies <dir> --count <dir> --out <cases.jsonl> [--max <n>]
 *       [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>]
 *       [--stop-after-sessions <n>] [--stop-zero-case-usd <n>] [--drop-navigation]
 *       [--store-only <file>]
 *   bun scripts/skill-cuts/cli.ts build-cases --config <file> --key-file <file>
 *       (--cut <id> | --set <id>) --skill <name> --old <dir> --copies <dir> --count <dir>
 *       --written <n> (--context-session <store>:<hash> | --sessions <file> --seed <n>)
 *       --out <cases.jsonl> [--session-cases <cases.jsonl>] [--reading-models <file>]
 *       [--model-pool <file>]... [--written-answer <file>] [--budget-usd <n>] [--drop-navigation]
 *   bun scripts/skill-cuts/cli.ts build-cases --config <file> --old <dir> --copies <dir>
 *       --count <dir> --reprofile <cases.jsonl> --out <new cases.jsonl>
 *   bun scripts/skill-cuts/cli.ts build-tasks --config <file> --key-file <file> --skill <dir>
 *       --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir>
 *       --copies <dir> --count <dir> --context-session <store>:<hash>
 *       [--test-setup <owner/repo>@<commit>:<path>]] [--max-candidates <n>]
 *       [--max-search-minutes <m>]
 *   bun scripts/skill-cuts/cli.ts build-tasks --config <file> --copies <dir> --count <dir>
 *       --reprofile <tasks.jsonl> --profiles-out <new dir> [--context-session <store>:<hash>]
 *   bun scripts/skill-cuts/cli.ts build-tasks --first-read --config <file> --key-file <file>
 *       --skill <dir> --sessions <file> --seed <n> [--max <n>] --out <tasks.jsonl>
 *       [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir>]
 *   bun scripts/skill-cuts/cli.ts results-index --count <set-count out dir> --nearest <dir>
 *       --cases <dir> --results <dir>
 *
 * `count-sql`, `copies`, `attribute`, `set-count`, `craft-lines`, `craft-merge`, `build-cases`,
 * `build-tasks` and `results-index` are the commands of countsql.ts, copies.ts, attribute.ts,
 * set-count.ts, craftlist.ts, craftmerge.ts, build.ts and results-index.ts, given their arguments
 * whole.
 *
 * **`behaviour`** runs the behaviour test of one skill: `--old` is the skill's own directory (the
 * old text: the skill at the cut's base, or a set's skill), and `--cases` a case file of that
 * skill (cases.ts, checked against `--old`). The old text is compared with one arm (score.ts's
 * Compared): `--new`, the skill's directory in the cut; `--ours`, a skill of ours whose name may
 * differ (arms.ts's `ours`); or, with neither, no skill. `--new` and `--ours` exclude each other.
 * Each stage records its
 * compared arm, and an `--out` whose results record another than the command's is refused.
 * Each case's results are one file, `<out>/cases/<id>.json` (CaseResult),
 * written whole (a fresh file renamed over it) after each round of runs, so a run started again
 * goes on from what was saved and skips each case whose stage is complete. `--after-case
 * <command>` runs `<command> <that file>` through `sh` when a case's stage ends; one still
 * running after 10 minutes (AFTER_CASE_TIMEOUT_MS) is ended, and counts as failed. In a run with
 * an arm of ours, every judgement reads the case's skill name and ours as `the-skill` (blind.ts);
 * with `--new` or no skill compared, the judge reads the case's text as it is.
 *
 * - `screen`: the session cases first, in the file's order, then the written ones (cases.ts's
 *   `caseKind`; score.ts's `screenOrder`), at most MAX_SCREENED_CASES screened cases of each kind,
 *   until the skill has MIN_INFORMATIVE_CASES informative cases in all (`screeningDone`): a case
 *   whose model the gateway does not serve
 *   (none of the configuration's `servedModels` has its id, its text after the last `/`) is
 *   dropped as `model-not-served`, and a case on a served model runs as that served
 *   `provider/model`, on the gateway route of its capture's body (gateway.ts); a case whose
 *   listing measure misses more than MAX_LISTING_CHANGE_PERCENT of its session's recorded names
 *   is dropped as `listing-misses`, before any call too, and so is a case whose capture builds
 *   its listing from the rebuilt profile and whose measure adds more names than that share of
 *   them, as `listing-adds` (score.ts's `listingStanding`); a case whose capture sends its
 *   session's recorded system prompt whole is not dropped for added names, since the model sees
 *   the recorded listing whatever the profile installs; the three
 *   judge controls (judge.ts's CONTROLS) run, and a case that fails any is dropped, its drop
 *   naming each control it failed; then a capture
 *   per arm (old, the compared arm unless it is none, and none; capture.ts, read back when saved
 *   already), SCREEN_RUNS replays of
 *   each, every reply of the round judged blind (all arms shuffled under a recorded seed), and
 *   rounds of extra runs for the runs with no verdict, as score.ts's `stageOutcome` says, at most
 *   MAX_EXTRA_RUNS per arm. With `--reading-models <file>` (attribute.ts's
 *   `<cut or set id>/models/<skill>.json` of the skill: its reading sessions by model), a skill
 *   whose reading sessions on a model the gateway serves are fewer than MIN_READING_SESSIONS gets
 *   no session case screened (score.ts's `servedScreeningRefusal`: `too few sessions on served
 *   models`, with the served and total counts): with no written case it is refused before any
 *   model call, gets no verdict, and the walk of the cut's ranking goes on to its next skill;
 *   with written cases, they are screened, and the refusal is never its reason for no verdict. A
 *   session case whose screen began before the refusal applied is passed over too: its runs count
 *   for nothing, and confirm, control and report take the screen as finished without it.
 * - `confirm`: FRESH_RUNS fresh runs of old and the compared arm for each flagged case that is not
 *   dropped. A confirm records the runner that made it (`runnerDigest`). Where a case's last two
 *   confirms at the same texts on one runner disagree, one a loss and one not, `confirm` runs a
 *   third, and score.ts pools the three (`confirmScore`). `--again <case id>` asks for one more
 *   confirm of a flagged case whose confirm is finished; it is refused (exit 2), naming the case,
 *   where the case already has POOLED_CONFIRMS confirms at these texts on this runner (no fourth
 *   runs), or two that agree (a third runs only where two disagree). `--confirm-flagged` confirms
 *   each flagged case that is not dropped whatever the skill's informative count or its other
 *   reasons for no verdict (`noVerdict`), so a case run again alone can be confirmed: the same
 *   confirm stage, third confirm and `--again` as without it. It never confirms a case its screen
 *   did not flag, and each confirm it runs records it (StageResult's `confirmFlagged`). With
 *   `--case <id>`, FRESH_RUNS fresh runs of old and the compared arm on that case alone, with no
 *   flag gate and no gate of MIN_INFORMATIVE_CASES informative cases, into its own `--out`; an id
 *   the case file does not hold, a case whose model the gateway does not serve, and `--again`
 *   beside it are refused.
 * - `control`: on the skill's control case, FRESH_RUNS runs each of old, `minus` (old less every
 *   line of the case's file that is the case's line, trimmed, or less every line that is
 *   `--planted-line`: a rule written twice is still the rule, so each copy goes; arms.ts) and
 *   `twin` (old again, its own capture: the false-loss control, which must be no loss). Old
 *   against `minus` is the planted-loss control, which must be a loss.
 *   confirm and control need the screen finished, and make no run when the skill can get no verdict
 *   (fewer than MIN_INFORMATIVE_CASES informative cases, a planted-loss control that is no loss,
 *   or a false-loss control that is a loss), except `confirm --confirm-flagged`.
 *   In any stage, a case one of whose captures needs a directory no read can give (context.ts's
 *   UnreadableDirectory: the commit holds no such directory, or GitHub cuts a level of its tree
 *   short even read alone) is dropped as `unreadable-directory`, its drop naming the repository,
 *   the commit and the path, and a run started again skips it; so is, as `legion-tool-undefined`,
 *   a case one of whose captures is of a Legion pane whose context installs pi-legion as an
 *   extension and whose body defines no `legion` tool in either of its two attempts (capture.ts's
 *   LegionToolUndefined), its drop naming the arm and each attempt's stderr lines. Each drop
 *   records its stage (`droppedAt`). A case dropped at screen is never informative; one dropped
 *   at confirm or
 *   control keeps its screen's passes in the score, so a flagged one that cannot be confirmed
 *   gives its skill no verdict unless another case is a confirmed loss, and a dropped control
 *   case gives its place to the next case of the control rule's order (score.ts's `scoreSkill`).
 *   A read that failed for another reason, after its retries, fails the command, and
 *   the next run reads it again. A case that holds a block of its skill its arms cannot each give
 *   their own text is dropped at screen, before any model call, as `injection-unrebuildable`, its
 *   reason naming each fault's kind (arms.ts's `injectionFaults`: `user-invoked`, a `/skill:`
 *   prompt the user typed; `other-shape`, a block with arguments or in another form;
 *   `plugin-block-misplaced`, a dispatch-first block away from where the plugin puts it), and the
 *   rest of the file runs; so is, at the stage whose capture finds it, a case whose subagent's
 *   agent autoloads the skill and whose messages hold no block of it (capture.ts's
 *   InjectionRefused, `autoload-no-block`). A case whose file read whole by path of its copy an
 *   arm's text (`--old`, `--new`, `--ours`) holds in a form the arms do not rebuild (build.ts's
 *   `pathFormIn`: arms.ts's `pathFormFault`, or a directory) is dropped at screen too, before
 *   any model call, as `path-form: <arm>: <fault>`, and the rest of the file runs: build-cases
 *   checks the old text and only a `--new` it was given, so a case built before the cut had a
 *   text reaches the screen unchecked against it, and every case reaches it unchecked against
 *   ours.
 * - Each stage and each run records the runner that made it (`runnerDigest`, the digest of this
 *   directory's code and of the ../skill-scenarios modules it imports: the commit is not knowable
 *   in a copy with no git): a run made before runs recorded it takes its stage's runner, and a
 *   stage made before stages did is the runner `unrecorded` (UNRECORDED_RUNNER). `screen`,
 *   `confirm` and `control` take a saved stage of their kind as done, or go on with one still
 *   running, only when each of its runs (or, with no run, the stage) is this runner's or that of
 *   a runner `--allow-runners <digest>[,<digest>...]` names; a saved stage of another runner is
 *   refused before any model call (exit 2), naming the case, its runner and this runner, and the
 *   `--allow-runners` that takes it with the runners already named (`checkRunners`), never
 *   skipped and never made again. `confirm` pools confirms on one runner at one set of texts as
 *   its rule says (score.ts's `confirmScore`).
 * - `report` (no model call) prints, for each stage kind, each runner with its case stages and
 *   runs (a stage whose runs two runners made counts under each, `runnerCounts`), and refuses
 *   (exit 2), naming each runner with its counts, results whose runs more than one runner made,
 *   unless `--allow-runners` names each, and then prints them as allowed. It prints the compared
 *   arm (no skill informs and clears nothing; ours is a bundle where it replaced a skill of our
 *   name that a profile held), each case's passes per arm and stage (and the history reads the
 *   ours arm removed), and for a dropped case the stage that dropped it, why, and the passes of
 *   each stage it ran; each confirm of a case with more than one, and the pooled passes of a case
 *   whose three confirms are pooled; the cases of each kind and the informative cases of each;
 *   the drops by the judge controls (and by each control), by `model-not-served`, by
 *   `unreadable-directory`, by `legion-tool-undefined`, by `listing-misses`, by `listing-adds`,
 *   by `injection-unrebuildable` (with the cases of each of its kinds), by `path-form` (by the
 *   arm its reason names: old and new, and ours where it is the compared arm), by
 *   `reads-past-last-round` and by `provider-refused`; the control case, and each dropped case it
 *   replaced; with ours compared, ours against old over all screened cases and over those with no
 *   removed read; each model of
 *   the cases; the skill's verdict and its reason (score.ts; `keeps` is printed "no loss detected
 *   by the registered rule", or "no loss of no skill …" with no skill compared), with how many of
 *   its screened cases have a listing that could not be
 *   measured, how many installed names their recorded listing gave them (`fromRecorded`), and how
 *   many rest on the assumption that their session had Dispatch; the rule's
 *   sensitivity (`lossChance` at an old pass rate of 0.9 and 0.7 and drops of 0.1 to 0.4), each
 *   screened case's smallest drop and their medians (score.ts's `dropFigures`), the estimated
 *   false-loss rate at the cases run (`falseLossChance`), the spend (`spendLines`: the sum of
 *   what the case results record their calls cost, part by part, and the request log's total and
 *   its difference from that sum, part by part, each part with its cause, where the log exists),
 *   the peak memory, how many captures' system-prompt digests differ from their session's, how
 *   many cases carry the Envoy plugin's block of the skill, by where the answer that their session
 *   had Dispatch comes from (arms.ts's `dispatchOf`: a Legion role, the transcript, or the
 *   assumption; each capture that carries the block records it in its stage's `captures`), for the
 *   cases whose capture sends their session's recorded system prompt whole (a profile with a
 *   `systemPrompt`), how many of their runs read a skill outside the recorded listing, by skill and
 *   by how the read was answered (score.ts's `outsideListing`: `copy`, an installed copy; `absent`,
 *   the unknown-skill reply), a count that drops no case and changes no verdict, and the `--limits`
 *   text as it is.
 *   Beside the verdict, as descriptions: each kind's screened cases with each arm's screen passes
 *   and the informative ones, the mean of old's screen pass rate less the compared arm's over the
 *   screened cases, all and each kind's, with its 95% t interval over the cases (score.ts's
 *   `meanDifference`), the screened cases that tie, floor, ceil and reverse (`screenShapes`), the
 *   inspection floors by kind with their ids and the mean over all screened cases and over those
 *   that are not floors (`inspectionLines`, checkout.ts's `readOnlyCall`), and
 *   the judge's identity (judge.ts's `judgeIdentity`) with how many stages record it; with
 *   `--build-summary` (build-cases' summaries, build.ts's `NavigationSummary`), each navigation
 *   line they name that no case has as its line (`untestedNavigationLine`), and how many
 *   replacement cases their session records name and how many of those the judge controls leave
 *   (`replacementLine`). Its compared arm is the one its results record. With
 *   `--reading-models`, it prints each model of the file with its sessions and
 *   whether the gateway serves it (the file's `served`), the sessions with no model and not
 *   queried, and the served models the screened session cases ran on, each with its cases; a
 *   verdict's line then ends with the reading sessions it speaks for, on each served model its
 *   screened session cases ran on (a model of the file counted under the served model of its id),
 *   and the ones it does not: on models the gateway does not serve, and on served models no
 *   screened session case ran on. A written case is a written task, never a reading session: the
 *   screened written cases are counted by model on a line of their own (`writtenTasksLine`), and a
 *   skill whose screened cases are all written speaks for no reading sessions. A skill the
 *   screen's refusal holds for gets no verdict. Without it, `report` says the
 *   reading sessions by model were not given, and its verdict names no models. A drop at confirm
 *   of a case its screen did not flag cannot happen, since only flagged cases are confirmed: a
 *   result that shows one fails `report`, and every stage command, naming the case (exit 1). A
 *   run any of whose confirms `confirm --confirm-flagged` made gives the case results as any run
 *   does, and the verdict line `case result only; no skill verdict stands (--confirm-flagged)`
 *   (CONFIRM_FLAGGED_VERDICT), never a skill verdict. With `--store-only <file>` (attribute.ts's
 *   `<cut id>/store-only/<skill>.json` of the skill), it prints, beside the reading sessions by
 *   model and whatever the verdict, the skill's counted sessions and how many of them only the
 *   transcript store holds, by host, which were never case sources; when a skill gets no verdict
 *   for fewer than MIN_INFORMATIVE_CASES informative cases and some are, the reason ends with
 *   their count (attribute.ts's `storeOnlyWords`). Without it, `report` says nothing of the store.
 *   For each stage kind it prints the deployments that answered each arm's runs, each run counted
 *   once (`<stage> deployments by arm: old <id> <n>, <id> <n>; new ...`, gateway.ts's
 *   `deploymentWords`; `not recorded` for results made before runs recorded them), and, for a
 *   kind with runs an arm's `--route-id` pin sent, `<stage> routing pinned: <arm>=<id>, ...` with
 *   their cases: such runs are a diagnostic, never a test result. For results `continue-mixed`
 *   wrote it also prints how many stages it wrote, from which `--from`, and which it selected; its
 *   runs of each kind by stage and arm (continued, waiting at the further-round limit, run again
 *   fresh, the fresh runs in their place, unchanged); and the continuations' spend and the fresh
 *   runs' apart (`continuedLines`).
 * - `audit-mixed` (no model call, no capture, no network; `auditMixedCommand`) applies the
 *   replay's mixed-reply rule (replay.ts's `replyBoundary` under the case's act, `caseAct`) to
 *   the judged reply of each saved run. It builds only the arms the stage ran, from `--old`
 *   and `--new` or `--ours` where given, and skips stages with other texts. It reads installed
 *   copies from the saved capture under `--cache-dir` or falls back to the case's profile
 *   (`profileInstalledCopies`). It counts checkout rounds separately from mixed replies.
 * - `continue-mixed` (`continueMixedCommand`) continues the saved runs of `--from`, a saved
 *   `--out`, that `audit-mixed` sorts `continues`, into a new `--out`: the cut review's rule counts a
 *   saved case only once those runs are continued. `--from` is only read, and `--out` may not be
 *   it. Before any model call it refuses a saved stage of another runner unless `--allow-runners`
 *   names it, as the stage commands do, and an `--only` line that names a stage no saved result
 *   holds. A run needs a session id, a saved capture, any earlier answered replies whole, and
 *   the deployment that answered its saved reply (the run's last
 *   `deployments` entry, or for a run saved before runs recorded one, the one deployment that
 *   answered every request of its session id in `<from>/requests.jsonl`, with that log's path and
 *   SHA-256; never a per-arm `--route-id`). The continuation is replay.ts's `resumeReplay`, the
 *   live replay's own code: replayOnce's first body with the run's own session id, the saved
 *   reply whole (its thinking signatures or encrypted reasoning) less the calls it left out, one
 *   answer per read, the capture's headers and the run's own routing id (none on the Responses
 *   route); later rounds follow the live rules, or stop after `--further-rounds <n>` requests,
 *   where a reply that would be continued again leaves the run with no verdict, its reason
 *   beginning `further-round-limit` (score.ts's FURTHER_ROUND_LIMIT), its reads and spend kept,
 *   and no fresh run in its place. Each request must be answered by the deployment that answered
 *   the saved reply (`x-litellm-model-id`). Each stage's continued replies are judged blind by the
 *   case's judge under a new seed, the stage's next round. A run that lacks a part, one a request
 *   of which another deployment answered, one whose request failed or was refused, and a judged
 *   run the rule leaves undecided, get no verdict and the stage's extra-run rule runs them again
 *   fresh; a judged run past the last round gets no verdict as `reads-past-last-round`. With
 *   `--only <file>` (each line `<case id>`, a tab, and `screen`, `control` or `confirm <n>`, the
 *   confirms numbered as `audit-mixed` numbers them) only the stages it names are continued; every
 *   run of the others is kept. Each case result in `--out` holds every stage of the saved one: the
 *   unchanged runs as saved, each continued run in its saved run's place naming that run (its
 *   number, blind id, round, verdict and runner), this runner, the deployment and where it came
 *   from, and its continuation's request ids (`continueMixed`), and the fresh runs; each stage
 *   records the `--from` it came from, its saved rounds and whether it was selected
 *   (`continued`). The saved captures its fresh runs replay are copied to `--out`, so the stage
 *   commands and `report` work on it. `--probe <case id>/<arm>/<run number>` rebuilds that one
 *   run's continuation and sends only its next request, with no later round and no judgement,
 *   and prints and writes to `<out>/probe.json` its request ids, its status or error, the
 *   deployment that answered it, its usage and its cost at list price; it refuses, before any
 *   call, a run the rule does not continue or whose record lacks a part, saying which.
 *
 * Every round of a stage sends the runs of all its arms in one order, seeded by the round's
 * recorded seed (replay.ts's `replayInterleaved`), each run with a session id of its own, and its
 * runs record that id and their place in the order. Each run of a Messages body also goes with a
 * routing id, on each of its requests (those of its skill-read and checkout rounds too) in the
 * header `x-litellm-session-id` (gateway.ts's ROUTING_HEADER), which the gateway pins a call's
 * deployment by: the runs numbered n of every arm of a case's stage share one, a random UUID drawn
 * once for n in each stage command, so old's, the compared arm's and none's run n (minus's and
 * twin's in a control) go to one deployment, every arm alike, and the run numbers fall on the
 * deployments as their ids hash. A Responses body goes with none. Each run records its routing id
 * (`routingId`, null for none) and the deployment that answered each of its requests
 * (`deployments`, gateway.ts's `deploymentOf`: the first 8 hex digits of `x-litellm-model-id`).
 * `--route-id <arm>=<uuid>` (repeatable) is a diagnostic: every run of that arm, in every round,
 * goes with that id in place of its round's, and records so (`routePinned`), while the arms it
 * does not name keep their rounds' ids; a pin of an arm the command does not run, or of an id that
 * is not a UUID, is refused (exit 2). A replay answers a reply's reads of skill files (the case's
 * skill from the arm, another skill of the profile from the copy its capture installed,
 * context.ts's `installedCopies` of the capture's record under `--cache-dir`). Before
 * replay, `restoredCapture` refuses a capture whose record names other copies than the case's
 * profile now gives, cached or not, and makes each missing copy again from its repository at its
 * commit. Each run records the answered rounds' reads (`skillReads`) and is judged on those
 * rounds' read calls and the reply after them. A reply that reads skill files beside other calls
 * is sorted by the case's act (replay.ts's `caseAct`, `callPart`): with an act among them it is
 * judged as it came (the run's `mixedAct`); with an undecided call it gets no verdict and is run
 * again, its reason beginning `mixed-undecided` and naming those calls; with only preparation its
 * reads are answered and it goes back holding only them, the other calls recorded as left out
 * (`leftOut`) and never run. `report` counts each kind by stage and arm.
 * A written case's runs also answer, from the shared checkout of its
 * repository at its commit under `--cache-dir` (`sharedCheckout`), at most 5 rounds of read-only
 * calls (replay.ts's checkout rounds), recorded in `checkoutRounds` and `skillReads` (a run saved
 * before the runner rebuilt the pin's summaries may mark a read of a code file it answered with
 * the file's whole text `wholeText`; no run marks one now); a written case with no checkout (built
 * before build-cases passed over a context session in no checkout it knows) answers none
 * (`NO_CHECKOUT`). A reply of a written case whose every call is read-only is such a round, never
 * a mixed reply the case's act sorts. A run whose reply after the last answered round still only
 * reads skill files, or reads them beside only preparation, or, in a written case, makes a
 * read-only call the checkout answers in no form, gets no verdict and is run again, its reason
 * beginning `reads-past-last-round` or `unanswered-read`; each case's line and `report` count
 * such runs by arm, `report` by kind of case too beside the saved reads marked `wholeText`, and
 * a stage left unscreened by an arm with such runs drops its case under
 * `reads-past-last-round`, with the
 * counts. A stage left unscreened in which the provider refused, on a content policy (a stream
 * error whose type ends in `_policy`, as `cyber_policy`), every replay that got no verdict in
 * each arm that lacks its judged runs can never be judged, and drops its case as
 * `provider-refused: provider refused every replay of <arms> (<policy>): <n> refusals (<arm>
 * <count>, ...)`, the refused arms joined by ` and `, and none named where every arm of the stage
 * is refused; an arm with its judged runs blocks nothing. A stage saved unscreened in that shape
 * before this rule, with no drop, is dropped so when a stage command reaches its case again, from
 * its saved runs and with no model call. A stage where an arm that lacks its runs has a run with
 * no verdict for another reason stays unscreened. A capture made for a stage shows the case's
 * session (capture.ts, "The harness strings"), whose head the capture reads once from its store
 * through the configuration's `psqlCommand` and saves under `<out>/sessions/`.
 *
 * Captures are saved under `<out>/captures/<arm>-<digest of the arm's skill directory>/`, so a new
 * text, or another planted line, is captured again. Making one runs Oh My Pi (`--omp`, else
 * `$LEGION_OMP_PATH`, else the `omp` on PATH, as an absolute path: `ompBinary`; a command given
 * `--models` when none of the three names one is refused) with `--models` (a `models.yml` whose
 * providers the capture points at its recorder), the context's repositories read with `gh` or
 * from `--repos <dir>` (context.ts's `localSource`), and the caches under `--cache-dir`; without
 * `--models` a missing capture fails the run. The peak resident memory (`VmHWM`, or the largest
 * `VmRSS` read each second on a kernel that keeps no `VmHWM`, as gVisor) is recorded at the end
 * of each case, and the run stops above 1 GiB.
 * `--budget-usd` stops new model calls once the replies' dollars at list price reach it; the runs
 * of the round so far are saved.
 *
 * **`read`** runs one reader of readers.ts (`runReader`) on the input `--in` holds, one JSON
 * value of
 * that reader's input schema, and writes `--out` whole: `{prompt, settings, answer, refusals,
 * spent, usd}`, `answer` null when every reply was refused.
 *
 * **`control`** runs one positive control of readers.ts (`runControl`) on the reader's input `--in`
 * holds, with what it plants: `map --delete-line <a line of the cut text the base text holds too>`,
 * `attack [--plant-line <line>]` (readers.ts's ATTACK_PLANTED_LINE by default), `same-job --planted
 * <file holding one skill entry {id, repo, name, description}> --copy-of <id>`, `sentence --into
 * <id> --sentence <text>`. It writes `--out` whole: `{control, held, why, answer, refusals, spent,
 * usd}`.
 *
 * **`craft`** runs the craft reader once (a fresh request, READER_TRIES tries) on one skill of
 * ours: every `.md` file of `--skill`, the style text's rules with their ids (style.ts), the craft
 * list's reader properties (craftlist.ts), and every `.md` file of each `--nearest` skill of
 * theirs; it reads nothing else. It writes the entry to `--out` (`craftRead`) and the exact user
 * message beside it, `<out less .json>.input.md`.
 *
 * **`nearest`** runs the nearest reader once on one skill (its name, whole description and
 * SKILL.md body) and every `<root>/<name>/SKILL.md` of each `--candidates <source>=<root>`, and
 * writes the row: the skill, the answer, and each named candidate with its whole description and
 * source.
 *
 * **`craft-coverage`** (no model call) lists each `<name>/SKILL.md` under `--root` less the
 * `--skip` names beside its craft entry in `--entries`, or none, and exits 1 naming each skill
 * with no entry.
 *
 * **`proposals`** reads each item of the `--list` lists of the `--section` section of a pull
 * request's body with the proposal-target and proposal readers (proposals.ts): one entry per item,
 * `<out>/<id>.json`, and the user messages sent, `<out>/<id>.input.md`. An item with an entry is
 * not read again; it exits 1 naming an item line it cannot parse, and when an item has no entry
 * at the end. `--dry-run` parses the body and prints the items, and reads nothing else.
 *
 * **`control craft`** writes a copy of the directory `--skill` names (the one a link points at, so
 * the skill itself is never written) with a section removed or list items joined, runs
 * `craft` on the copy and on the unchanged skill, takes each side's candidates (gaps in its line
 * range, of its kinds and rules), asks the claim reader once a candidate where the side gives a
 * claim, and holds only when the copy has a candidate that states its claim (any, with none) and
 * the unchanged skill has none that does (none at all, with none). It writes every input and
 * answer under `--out`.
 *
 * **`count-sql`** fills the count, models, roots and hashes SQL for Postgres, or the count and
 * coverage SQL for ClickHouse. With `--run` it sends a ClickHouse query to `transcriptStore.command`.
 *
 * **`load`** is load.ts's load test: per task, the host's first request captured once
 * and sent `--runs` times (LOAD_RUNS by default), the runs of a profile's tasks in one seeded
 * order, each with a session id of its own and a routing id derived from `--route-seed`, its task
 * and its run number (load.ts); each run's first assistant message read for a read of
 * the skill, the loads and verdicts printed and saved under `--out`, which a run started again
 * goes on from. The model is LOAD_MODEL and the thinking level LOAD_THINKING unless `--model` and
 * `--thinking` say otherwise. `--context-session <store>:<path hash>` names the session a written
 * task stands for in its capture. On Oh My Pi, a reply that reads skill files beside other calls
 * is sorted by replay.ts's LOAD_RULE (load.ts). **`load audit-mixed`** (no model call;
 * `loadAuditCommand`) applies that rule to the saved runs of a load's `--out`, and prints and
 * writes to `<out>/audit-mixed.json`, per task, the runs that would end at an action, continue,
 * or be left with no verdict, with their run numbers and session ids.
 *
 * Each model request goes through the request log (`loggedGateway`): one JSON line per request,
 * appended to `--request-log` (by default `<out>/requests.jsonl` for `behaviour`, `load`,
 * `control craft` and `proposals`, and the `--out` file's path with `.requests.jsonl` added for
 * `read`, `control`, `craft` and `nearest`), holding what the call was for, when it was asked for
 * and sent and the calls in flight then, the session id it went with, the model, the thinking type
 * and budget, max_tokens, the temperature, the effort, and the reply's answering model, ids,
 * gateway headers, cache read and write tokens, usage and dollars at list price; never the key and
 * never a message's text.
 *
 * Exit status: 0 done (a control held); 1 failed (a reader's every reply refused, a control that
 * did not hold, a call, a load test's task or an `--after-case` command that failed, a proposals
 * item line that does not parse or an item left with no entry, a skill `craft-coverage` finds with
 * no entry, or behaviour results that show a drop no stage makes); 2 refused before any model
 * call; 3 stopped (the budget is spent, the peak memory passed 1 GiB, 1.5 GiB for `copies`, or the
 * listing rule dropped most of a load test's tasks); 4 stopped by build-cases' stop rule. The
 * commands of the other files return their own status, which `main` returns as it is.
 */
import { createHash, randomBytes, randomUUID } from "node:crypto";
import {
  appendFileSync,
  closeSync,
  copyFileSync,
  cpSync,
  existsSync,
  fstatSync,
  linkSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  writeSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { MessagesApiError } from "../skill-scenarios/messages-api";
import { outRefusal } from "../skill-scenarios/production-dispatch";
import { processSpendRecorder, type SpendRecorder } from "../skill-scenarios/spend-record";
import { readTargets } from "../skill-scenarios/transcript";
import {
  type Arm,
  type ArmDirs,
  type ArmName,
  buildArm,
  InjectionRefused,
  type InstalledCopy,
  injectionFaults,
} from "./arms";
import {
  main as attributeMain,
  type ModelCount,
  modelCountOf,
  modelId,
  readStoreOnly,
  type StoreOnly,
  storeOnlyHosts,
  storeOnlyWords,
} from "./attribute";
import {
  main as buildMain,
  fromRecordedIn,
  type NavigationCounts,
  NavigationSummary,
  pathFormIn,
  processLives,
} from "./build";
import {
  type Capture,
  captureBody,
  capturePaths,
  gatewayHints,
  LEGION_TOOL_UNDEFINED,
  LegionToolUndefined,
  listingOf,
  restoredCapture,
  type SessionFacts,
  savedCapture,
  sessionFacts,
  sessionHeads,
  userTurnKey,
} from "./capture";
import {
  type Case,
  caseKind,
  insideRoot,
  loadCases,
  pathReadFile,
  stepSkillFiles,
  Thinking,
  testedSource,
  textLines,
} from "./cases";
import {
  BASH_GATE_COMMITS,
  type BashInterceptor,
  type CheckoutCopy,
  colocateJj,
  type DotfilesCheckout,
  gateVersion,
  interceptorOf,
  type KnivesRepository,
  messagesRunJj,
  readOnlyCall,
  removeOutsideLinks,
  type ShellSetup,
  sessionHome,
  shellPrefixOf,
  type Unanswered,
} from "./checkout";
import {
  configFrom,
  counted,
  fileDigests,
  firstLine,
  json,
  OneLine,
  ompSkillName,
  ompText,
  parse,
  pastLimit,
  Refused,
  readFrontmatter,
  readJson,
  required,
  Stopped,
  schemaFaults,
  sha256,
  splitFrontmatter,
  textOf,
  timeLimit,
  writeWhole,
} from "./common";
import { type SkillCutsConfig, storeNamed } from "./config";
import {
  ghSource,
  installedCopies,
  localSource,
  NoSuchFile,
  profileCopies,
  type RepoSource,
  repositoryDirectory,
  repositoryFile,
  repositoryTree,
  UNREADABLE_DIRECTORY,
  UnreadableDirectory,
} from "./context";
import { main as copiesMain, spawnGh } from "./copies";
import { main as countSqlMain } from "./countsql";
import { main as craftLinesMain, readCraftList } from "./craftlist";
import { main as craftMergeMain } from "./craftmerge";
import {
  BudgetSpent,
  bodyIdentity,
  createGateway,
  deploymentOf,
  deploymentWords,
  type Gateway,
  isUuid,
  PRICE_SOURCE,
  PRICES,
  type Reply,
  type RequestBody,
  ROUTING_HEADER,
  type Sent,
  type Spent,
  SpentRecord,
  STREAM_FAILED,
  toolCalls,
  usdOf,
} from "./gateway";
import {
  type ArmReply,
  CONTROLS,
  ControlName,
  type Judged,
  JudgeIdentity,
  judgeBlind,
  judgeControls,
  judgeIdentity,
  newSeed,
} from "./judge";
import {
  auditLoadMixed,
  Host,
  jsonLines,
  LOAD_MODEL,
  LOAD_THINKING,
  runLoad,
  skillName,
  type TaskAudit,
  TEST_SETUP_LABEL,
  testSetupOption,
} from "./load";
import { mib, procPeakRssBytes, RUN_MEMORY_LIMIT_BYTES, startPeakRssSampling } from "./memory";
import {
  type DiffFile,
  FindingsEntry,
  fencedLines,
  ItemFaults,
  idPrefix,
  type ProposalItem,
  type ProposalsRun,
  type PullDiff,
  parseProposalItems,
  pendingItems,
  rulesRecords,
  runProposals,
  splitDiff,
} from "./proposals";
import {
  ATTACK_PLANTED_LINE,
  type ClaimAnswer,
  ClaimInput,
  type Control,
  type CraftAnswer,
  CraftGap,
  type CraftInput,
  craftInput,
  loadPrompt,
  NEAREST_RULE,
  type NearestAnswer,
  plantControl,
  READER_NAMES,
  READERS,
  ReaderFailed,
  type ReaderName,
  runControl,
  runReader,
  SameJobInput,
  skillFilePaths,
  skillFiles,
} from "./readers";
import {
  AnsweredRead,
  type Boundary,
  type CallPart,
  CheckoutRead,
  caseAct,
  FurtherRoundLimit,
  LeftOutCall,
  MAX_CHECKOUT_ROUNDS,
  MAX_SKILL_ROUNDS,
  MIXED_UNDECIDED,
  type MixedRule,
  NotResumable,
  READS_PAST_LAST_ROUND,
  ReadsPastLastRound,
  type Replayed,
  replayInterleaved,
  replyBoundary,
  resumeReplay,
  type SavedReply,
  type SkillRead,
  type SortedCall,
  sendOrder,
  UNANSWERED_READ,
  UnansweredRead,
  UndecidedReply,
} from "./replay";
import { main as resultsIndexMain } from "./results-index";
import {
  type CaseDrop,
  type CaseFacts,
  CaseKind,
  type CaseScore,
  Compared,
  DROP_CHANCE,
  DROP_STEP_POINTS,
  dropFigures,
  FRESH_RUNS,
  FURTHER_ROUND_LIMIT,
  isControlLoss,
  isFalseLoss,
  isFlagged,
  isInformative,
  isLoss,
  LISTING_ADDS,
  LISTING_MISSES,
  LOAD_RUNS,
  LOSS_GAP,
  listingStanding,
  lossChance,
  MAX_EXTRA_RUNS,
  MAX_SCREENED_CASES,
  type MadeConfirm,
  MIN_INFORMATIVE_CASES,
  meanDifference,
  NO_CONTROL_LOSS,
  type NoVerdict,
  noControlLoss,
  outsideListing,
  outsideListingWords,
  type Passes,
  POOLED_CONFIRMS,
  RunRecord,
  SCREEN_RUNS,
  type SkillScore,
  type SmallestDrop,
  STAGE_RUNS,
  Stage,
  type StageOutcome,
  StageState,
  scoreSkill,
  screenCounts,
  screeningDone,
  screenOrder,
  screenShapes,
  servedScreeningRefusal,
  skillVerdict,
  stageArms,
  stageOutcome,
} from "./score";
import { main as setCountMain } from "./set-count";
import { type StyleRule, styleRules } from "./style";

/** The longest one try of a model call may take: a reply with a long thinking budget is slow. */
const CALL_TIMEOUT_MS = 600_000;
/** The model calls in flight at once, unless `--concurrency` says otherwise. */
const DEFAULT_CONCURRENCY = 8;
/** The longest one `--after-case` command may run. It is the user's command, so its time is
 *  theirs; what it is for, a copy of the case's results file elsewhere, took 59 ms on the pod for
 *  a file of 2.2 MB (the largest results file the efficiency review found was 2.1 MB; measured
 *  2026-11-23), and 10 minutes leaves room for a copy to another machine. */
export const AFTER_CASE_TIMEOUT_MS = 600_000;
/** The request log's name: `<out>/requests.jsonl` for `behaviour`, `load`, `control craft` and
 *  `proposals`, and the `--out` file's path with `.requests.jsonl` added for `read`, `control`,
 *  `craft` and `nearest`, unless `--request-log` names one. */
const REQUEST_LOG = "requests.jsonl";
/** The directories under the system's temporary directory that `--scratch` and `--cache-dir`
 *  name unless given. */
const SCRATCH_DIR = "skill-cuts-scratch";
const CACHE_DIR = "skill-cuts-cache";

/** Exit statuses. */
const EXIT = { ok: 0, failed: 1, refused: 2, stopped: 3 } as const;

/** What the commands write to, and the process facts they read. */
export interface CliIo {
  /** Writes `text` to standard output as it is. */
  out(text: string): void;
  /** Writes one line to standard error. */
  err(line: string): void;
  /** The process's peak resident memory so far, in bytes (`procPeakRssBytes`). */
  peakRssBytes(): number;
  /** Where each model answer records its usage span (spend-record.ts; `processSpendRecorder()` in
   *  a real run), flushed before `main` returns; none when absent. */
  readonly spendRecorder?: SpendRecorder;
}

/** The io of a real run. Where the kernel keeps no `VmHWM`, the resident memory is read every
 *  second from now on (memory.ts), so that the peak misses little between the ends of cases. */
function processIo(): CliIo {
  startPeakRssSampling();
  return {
    out: (text) => process.stdout.write(text),
    err: (line) => process.stderr.write(`${line}\n`),
    peakRssBytes: procPeakRssBytes,
    spendRecorder: processSpendRecorder(),
  };
}

// The request log.

/** A field of an object, or undefined. */
function field(value: unknown, key: string): unknown {
  return typeof value === "object" && value !== null
    ? (value as Record<string, unknown>)[key]
    : undefined;
}

/** The settings of a request body that the request log records: its model, thinking type and
 *  budget, max_tokens (a Responses body's `max_output_tokens`), temperature, effort (a Responses
 *  body's `reasoning.effort`), and whether it streams. Nothing else of the body. */
function requestSettings(body: RequestBody): Record<string, unknown> {
  return {
    model: body.model,
    thinking: field(body.thinking, "type") ?? null,
    thinking_budget: field(body.thinking, "budget_tokens") ?? null,
    max_tokens: body.max_tokens ?? body.max_output_tokens ?? null,
    temperature: body.temperature ?? null,
    effort: field(body.output_config, "effort") ?? field(body.reasoning, "effort") ?? null,
    stream: body.stream === true,
  };
}

/** `gateway`, with each request it sends appended to `logFile` as one JSON line: when it was
 *  asked for (`at`), when it got its place and was sent (`sentAt`) and how many calls were in
 *  flight then, it among them (`inFlight`), `kind` (what the call is for), the body's settings
 *  (`requestSettings`) and the session id it went with (`userId`: its `metadata.user_id` or
 *  `prompt_cache_key`), and the reply's model (`answeredBy`), message id, request id, provider
 *  request id, the gateway's headers of it (`gateway`: gateway.ts's GATEWAY_HEADERS it came
 *  with, the deployment and model group among them), duration, stop reason, cache read and write
 *  tokens, usage and dollars; or, for a call that failed, the error's name and HTTP status. Never
 *  the key, and never a message's text. */
function loggedGateway(gateway: Gateway, logFile: string, kind: string): Gateway {
  mkdirSync(path.dirname(logFile), { recursive: true });
  const log = (line: Record<string, unknown>) =>
    appendFileSync(logFile, `${JSON.stringify(line)}\n`);
  return {
    async send(body, options) {
      const at = new Date().toISOString();
      const settings = requestSettings(body);
      const userId = bodyIdentity(body);
      let sent: Sent | undefined;
      const onSent = (each: Sent): void => {
        sent = each;
        options?.onSent?.(each);
      };
      try {
        const reply = await gateway.send(body, { ...options, onSent });
        const id: unknown = reply.message.id;
        log({
          at,
          sentAt: reply.sentAt,
          inFlight: reply.inFlight,
          kind,
          ...settings,
          userId,
          answeredBy: reply.message.model,
          messageId: typeof id === "string" ? id : null,
          requestId: reply.requestId ?? null,
          providerRequestId: reply.providerRequestId,
          gateway: reply.gatewayHeaders,
          durationMs: reply.durationMs,
          stopReason: reply.message.stop_reason,
          cacheRead: reply.spent.cacheRead,
          cacheWrite: reply.spent.cacheWrite,
          usage: reply.message.usage,
          usd: reply.spent.usd,
        });
        return reply;
      } catch (error) {
        log({
          at,
          sentAt: sent?.at ?? null,
          inFlight: sent?.inFlight ?? null,
          kind,
          ...settings,
          userId,
          error: error instanceof Error ? error.name : "error",
          status: error instanceof MessagesApiError ? (error.status ?? null) : null,
        });
        throw error;
      }
    },
    spend: () => gateway.spend(),
  };
}

// Options every model command reads.

/** The options of a command that calls a model. */
const GATEWAY_OPTIONS = {
  config: { type: "string" },
  "key-file": { type: "string" },
  "budget-usd": { type: "string" },
  concurrency: { type: "string" },
  "request-log": { type: "string" },
} as const;

/** The gateway a command's options name: the configuration's address, the key file, the budget
 *  and the concurrency, its answers' usage recorded with `spendRecorder` (none when undefined).
 *  Throws a refusal on an option out of range. */
function openGateway(values: {
  config: SkillCutsConfig;
  keyFile: string;
  budgetUsd?: string;
  concurrency?: string;
  spendRecorder: SpendRecorder | undefined;
}): Gateway {
  const concurrency =
    values.concurrency === undefined ? DEFAULT_CONCURRENCY : Number(values.concurrency);
  if (!Number.isInteger(concurrency) || concurrency < 1) {
    throw new Refused(`--concurrency ${values.concurrency} is not a whole number of 1 or more`);
  }
  let budgetUsd: number | undefined;
  if (values.budgetUsd !== undefined) {
    budgetUsd = Number(values.budgetUsd);
    if (values.budgetUsd.trim() === "" || !Number.isFinite(budgetUsd) || budgetUsd < 0) {
      throw new Refused(`--budget-usd ${values.budgetUsd} is not a number of dollars of 0 or more`);
    }
  }
  return createGateway({
    baseUrl: values.config.gatewayBaseUrl,
    keyFile: values.keyFile,
    concurrency,
    timeoutMs: CALL_TIMEOUT_MS,
    ...(budgetUsd === undefined ? {} : { budgetUsd }),
    spendRecorder: values.spendRecorder ?? null,
  });
}

/** `openGateway` of a model command's parsed options: the configuration `config`, the key file
 *  `keyFile`, `values`' `--budget-usd` and `--concurrency`, and the command's `spendRecorder`
 *  (CliIo's). */
function gatewayOf(
  config: SkillCutsConfig,
  keyFile: string,
  values: { readonly "budget-usd"?: string; readonly concurrency?: string },
  spendRecorder: SpendRecorder | undefined
): Gateway {
  return openGateway({
    config,
    keyFile,
    budgetUsd: values["budget-usd"],
    concurrency: values.concurrency,
    spendRecorder,
  });
}

/** The Oh My Pi binary a capture runs, as an absolute path: `given` (`--omp`), else
 *  `env.LEGION_OMP_PATH`, else `omp`; a name with no `/` is looked up on `env.PATH`, since a
 *  capture opens the binary as a file (its natives are linked from it). Undefined when the one
 *  that applies names no file. */
export function ompBinary(
  given: string | undefined,
  env: Readonly<Record<string, string | undefined>> = process.env
): string | undefined {
  const name =
    given ??
    (env.LEGION_OMP_PATH === undefined || env.LEGION_OMP_PATH === "" ? "omp" : env.LEGION_OMP_PATH);
  if (!name.includes("/")) return Bun.which(name, { PATH: env.PATH ?? "" }) ?? undefined;
  const full = path.resolve(name);
  return statSync(full, { throwIfNoEntry: false })?.isFile() ? full : undefined;
}

/** `ompBinary(given)`, or a refusal naming where it looked. */
function requiredOmp(given: string | undefined): string {
  const found = ompBinary(given);
  if (found !== undefined) return found;
  const where =
    given !== undefined
      ? `--omp ${given} names no file`
      : process.env.LEGION_OMP_PATH
        ? `LEGION_OMP_PATH ${process.env.LEGION_OMP_PATH} names no file`
        : "no --omp is given, LEGION_OMP_PATH is unset, and no omp is on PATH";
  throw new Refused(
    `--models makes captures, which run Oh My Pi, and none is found: ${where}. Give --omp <absolute path>, or set LEGION_OMP_PATH`
  );
}

// `read`.

const READ_USAGE =
  "usage: cli.ts read --config <skill-cuts.json> --key-file <file> --prompt <name> --in <input.json> --out <answer.json> [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]";

/** `skill-cuts read`. */
async function readCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(
    argv,
    {
      ...GATEWAY_OPTIONS,
      prompt: { type: "string" },
      in: { type: "string" },
      out: { type: "string" },
    },
    READ_USAGE
  );
  const given = required(values, ["config", "key-file", "prompt", "in", "out"], READ_USAGE);
  if (!(READER_NAMES as readonly string[]).includes(given.prompt)) {
    throw new Refused(
      `--prompt ${given.prompt} is no reader; the readers are ${READER_NAMES.join(", ")}`
    );
  }
  const name = given.prompt as ReaderName;
  const checked = READERS[name].input.safeParse(readJson(given.in, "the input"));
  if (!checked.success) {
    throw new Refused(
      `the ${name} reader's input ${given.in} is refused: ${schemaFaults(checked.error)}`
    );
  }
  const config = configFrom(given.config);
  const settings = loadPrompt(name).settings;
  const gateway = openGateway({
    config,
    keyFile: given["key-file"],
    budgetUsd: values["budget-usd"],
    concurrency: values.concurrency,
    spendRecorder: io.spendRecorder,
  });
  const logged = loggedGateway(
    gateway,
    values["request-log"] ?? `${given.out}.${REQUEST_LOG}`,
    `reader:${name}`
  );
  try {
    const result = await runReader(name, checked.data, logged);
    const usd = usdOf(result.spent);
    writeWhole(given.out, json({ prompt: name, settings, ...result, usd }));
    io.out(
      `read: the ${name} reader answered after ${result.refusals.length} refused replies, $${usd.toFixed(4)} at list price; written to ${given.out}\n`
    );
    return EXIT.ok;
  } catch (error) {
    if (!(error instanceof ReaderFailed)) throw error;
    const usd = usdOf(error.spent);
    writeWhole(
      given.out,
      json({
        prompt: name,
        settings,
        answer: null,
        refusals: error.refusals,
        spent: error.spent,
        usd,
      })
    );
    io.err(`read: ${error.message}; written to ${given.out}`);
    return EXIT.failed;
  }
}

// `control`.

const CONTROL_USAGE = [
  "usage: cli.ts control <kind> --config <skill-cuts.json> --key-file <file> --in <input.json> --out <result.json> [--request-log <file>] [--budget-usd <n>] [--concurrency <n>], where <kind> and what it plants is one of:",
  "  map --delete-line <a line of the cut text that the base text holds too>",
  "  attack [--plant-line <line>]",
  "  same-job --planted <file holding one skill entry {id, repo, name, description}> --copy-of <id>",
  "  sentence --into <id> --sentence <text>",
  "or the craft reader's control, which takes its own flags: control craft (--remove-section <heading> | --join-lines <a>-<b>) ...",
].join("\n");

const CONTROL_KINDS = ["map", "attack", "same-job", "sentence"] as const;
type ControlKind = (typeof CONTROL_KINDS)[number];

/** The control `kind` on the reader input `input`, with what `values` plant. Throws a refusal. */
function controlOf(
  kind: ControlKind,
  input: unknown,
  values: Record<string, string | undefined>
): Control {
  const checked = READERS[kind].input.safeParse(input);
  if (!checked.success) {
    throw new Refused(`the ${kind} reader's input is refused: ${schemaFaults(checked.error)}`);
  }
  switch (kind) {
    case "map": {
      const { "delete-line": deleteLine } = required(values, ["delete-line"], CONTROL_USAGE);
      return { reader: "map", input: READERS.map.input.parse(input), deleteLine };
    }
    case "attack":
      return {
        reader: "attack",
        input: READERS.attack.input.parse(input),
        plantLine: values["plant-line"] ?? ATTACK_PLANTED_LINE,
      };
    case "same-job": {
      const given = required(values, ["planted", "copy-of"], CONTROL_USAGE);
      const planted = SameJobInput.shape.skills.element.safeParse(
        readJson(given.planted, "the planted skill")
      );
      if (!planted.success) {
        throw new Refused(
          `the planted skill ${given.planted} is not one skill entry {id, repo, name, description}`
        );
      }
      return {
        reader: "same-job",
        input: READERS["same-job"].input.parse(input),
        planted: planted.data,
        copyOf: given["copy-of"],
      };
    }
    case "sentence": {
      const given = required(values, ["into", "sentence"], CONTROL_USAGE);
      return {
        reader: "sentence",
        input: READERS.sentence.input.parse(input),
        into: given.into,
        sentence: given.sentence,
      };
    }
  }
}

/** `skill-cuts control <kind>`; `control craft` is `controlCraftCommand`. */
async function controlCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const [kind, ...rest] = argv;
  if (kind === "craft") return await controlCraftCommand(rest, io);
  if (!(CONTROL_KINDS as readonly (string | undefined)[]).includes(kind)) {
    throw new Refused(`control ${kind ?? ""}: no such control. ${CONTROL_USAGE}`);
  }
  const values = parse(
    rest,
    {
      ...GATEWAY_OPTIONS,
      in: { type: "string" },
      out: { type: "string" },
      "delete-line": { type: "string" },
      "plant-line": { type: "string" },
      planted: { type: "string" },
      "copy-of": { type: "string" },
      into: { type: "string" },
      sentence: { type: "string" },
    },
    CONTROL_USAGE
  );
  const given = required(values, ["config", "key-file", "in", "out"], CONTROL_USAGE);
  const control = controlOf(kind as ControlKind, readJson(given.in, "the input"), values);
  try {
    plantControl(control);
  } catch (error) {
    throw new Refused(`the ${kind} control cannot be planted: ${messageFor(error)}`);
  }
  const config = configFrom(given.config);
  const gateway = openGateway({
    config,
    keyFile: given["key-file"],
    budgetUsd: values["budget-usd"],
    concurrency: values.concurrency,
    spendRecorder: io.spendRecorder,
  });
  const logged = loggedGateway(
    gateway,
    values["request-log"] ?? `${given.out}.${REQUEST_LOG}`,
    `control:${kind}`
  );
  try {
    const ran = await runControl(control, logged);
    const usd = usdOf(ran.result.spent);
    writeWhole(
      given.out,
      json({ control: kind, held: ran.held, why: ran.why, ...ran.result, usd })
    );
    const line = `control ${kind}: ${ran.held ? "held" : "did not hold"}: ${ran.why}; $${usd.toFixed(4)} at list price; written to ${given.out}`;
    if (ran.held) io.out(`${line}\n`);
    else io.err(line);
    return ran.held ? EXIT.ok : EXIT.failed;
  } catch (error) {
    if (!(error instanceof ReaderFailed)) throw error;
    const usd = usdOf(error.spent);
    writeWhole(
      given.out,
      json({
        control: kind,
        held: false,
        why: error.message,
        answer: null,
        refusals: error.refusals,
        spent: error.spent,
        usd,
      })
    );
    io.err(`control ${kind}: did not hold: ${error.message}; written to ${given.out}`);
    return EXIT.failed;
  }
}

// `craft`, `nearest`, `craft-coverage` and `control craft`: the readers of our skills.

/** Whether `file` is a file; false where it, or a directory on its path, is none. */
function isFile(file: string): boolean {
  try {
    return statSync(file).isFile();
  } catch {
    return false;
  }
}

/** `value` when it is a string that is not empty, else `otherwise`: a frontmatter field. */
function textOr(value: unknown, otherwise: string): string {
  return typeof value === "string" && value !== "" ? value : otherwise;
}

/** A skill directory as the readers read it: its name (common.ts's `ompSkillName`: its
 *  frontmatter `name`, trimmed, else the directory's) and every `.md` file under it
 *  (`skillFiles`), SKILL.md first. */
interface ReadSkill {
  readonly dir: string;
  readonly name: string;
  readonly files: readonly { readonly path: string; readonly text: string }[];
}

/** The skill in `dir`, which the flag `flag` names. Throws a refusal when `dir` cannot be read or
 *  holds no SKILL.md. */
function readSkill(dir: string, flag: string): ReadSkill {
  let files: ReadSkill["files"];
  try {
    files = skillFiles(dir);
  } catch (error) {
    throw new Refused(`${flag} ${dir} cannot be read: ${messageFor(error)}`);
  }
  const skill = files.find((file) => file.path === "SKILL.md");
  if (skill === undefined) throw new Refused(`${flag} ${dir} has no SKILL.md`);
  return {
    dir,
    name: ompSkillName(readFrontmatter(ompText(skill.text)), path.basename(dir)),
    files,
  };
}

/** The skills directly under `root`: each `<root>/<name>/SKILL.md`, by name. Throws a refusal
 *  when `root` cannot be read or holds none. */
function skillDirs(root: string): string[] {
  let names: string[];
  try {
    names = readdirSync(root);
  } catch (error) {
    throw new Refused(`cannot read the root ${root}: ${messageFor(error)}`);
  }
  const skills = names.filter((name) => isFile(path.join(root, name, "SKILL.md"))).sort();
  if (skills.length === 0) throw new Refused(`the root ${root} holds no <name>/SKILL.md`);
  return skills;
}

/** The file a reader's exact user message is written to: `out` less `.json`, and `.input.md`. */
function inputFile(out: string): string {
  return `${out.replace(/\.json$/, "")}.input.md`;
}

/** The bytes of `file`, which is `what`. Throws a refusal naming it. */
function readBytes(file: string, what: string): Buffer {
  try {
    return readFileSync(file);
  } catch (error) {
    throw new Refused(`cannot read ${what} ${file}: ${messageFor(error)}`);
  }
}

/** The two sides of `value`, the value of `flag` in the shape `shape` (`<a>=<b>`): the text before
 *  its first `=`, trimmed, and the rest. Throws a refusal when either side is empty. */
function pairOf(flag: string, value: string, shape: string): [string, string] {
  const at = value.indexOf("=");
  const left = value.slice(0, Math.max(at, 0)).trim();
  const right = value.slice(at + 1);
  if (at < 0 || left === "" || right === "") throw new Refused(`${flag} ${value} is not ${shape}`);
  return [left, right];
}

const CRAFT_OPTIONS = {
  ...GATEWAY_OPTIONS,
  skill: { type: "string" },
  "skill-source": { type: "string" },
  style: { type: "string" },
  "style-source": { type: "string" },
  craft: { type: "string" },
  "craft-source": { type: "string" },
  nearest: { type: "string", multiple: true },
  out: { type: "string" },
} as const;
const CRAFT_REQUIRED = [
  ...["config", "key-file", "skill", "style", "style-source", "craft", "craft-source"],
  "out",
] as const;
const CRAFT_FLAGS =
  "--config <skill-cuts.json> --key-file <file> --skill <dir> --style <file> --style-source <text> --craft <craft-list.json> --craft-source <text> [--nearest <dir>]... [--skill-source <text>]";
const CRAFT_USAGE = `usage: cli.ts craft ${CRAFT_FLAGS} --out <entry.json> [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]`;

/** The style text and the craft list a reader reads (`--style` and `--craft`), each with its file,
 *  the words its `--*-source` flag gives it, and its SHA-256: `craft`'s and `proposals`'. */
type RulesReads = Pick<ProposalsRun, "style" | "craft">;

/** What a craft read reads, all of it before any call: the skill, the style text and its rules,
 *  the craft list, and each nearest skill of theirs, each with its source where it has one. */
interface CraftReads extends RulesReads {
  readonly skill: ReadSkill;
  readonly skillSource: string | null;
  readonly nearest: readonly ReadSkill[];
}

/** The reads of `--style` and `--craft`. Throws a refusal: a file it cannot read, a style text with
 *  no rule, a craft list the shape refuses. */
function rulesReads(
  given: Record<"style" | "style-source" | "craft" | "craft-source", string>
): RulesReads {
  const styleBytes = readBytes(given.style, "the style text");
  let rules: StyleRule[];
  try {
    rules = styleRules(styleBytes.toString("utf8"));
  } catch (error) {
    throw new Refused(`the style text ${given.style} is refused: ${messageFor(error)}`);
  }
  const craftList = readCraftList(given.craft);
  return {
    style: { file: given.style, source: given["style-source"], sha256: sha256(styleBytes), rules },
    craft: {
      file: given.craft,
      source: given["craft-source"],
      sha256: sha256(craftList.bytes),
      list: craftList.list,
    },
  };
}

/** The reads of `craft`'s flags. Throws a refusal: a file it cannot read, a style text with no
 *  rule, a craft list the shape refuses, a skill directory with no SKILL.md. */
function craftReads(
  given: Record<"skill" | "style" | "style-source" | "craft" | "craft-source", string>,
  values: { readonly "skill-source"?: string; readonly nearest?: readonly string[] }
): CraftReads {
  return {
    ...rulesReads(given),
    skill: readSkill(given.skill, "--skill"),
    skillSource: values["skill-source"] ?? null,
    nearest: (values.nearest ?? []).map((dir) => readSkill(dir, "--nearest")),
  };
}

/** The craft reader's input of `reads`, with `skill` in place of its skill when given. Throws a
 *  refusal when the reader's input schema refuses it. */
function craftReaderInput(reads: CraftReads, skill: ReadSkill = reads.skill): CraftInput {
  const checked = READERS.craft.input.safeParse(
    craftInput({
      skill: skill.name,
      files: skill.files,
      style: reads.style.rules,
      list: reads.craft.list,
      nearest: reads.nearest,
    })
  );
  if (!checked.success) {
    throw new Refused(`the craft reader's input is refused: ${schemaFaults(checked.error)}`);
  }
  return checked.data;
}

/** One fresh request of the reader `name` on `input`, its entry written whole to `out`: `entry`,
 *  then the answer (null when every reply was refused), what `after` gives for that answer, the
 *  refusals, the spend and its dollars at list price. Returns the answer and its dollars, or the
 *  ReaderFailed of a reader whose every reply was refused; any other failure throws. */
async function readToFile<A>(
  name: ReaderName,
  input: unknown,
  gateway: Gateway,
  out: string,
  entry: Record<string, unknown>,
  after: (answer: A | null) => Record<string, unknown> = () => ({})
): Promise<{ answer: A; usd: number } | { answer: undefined; failed: ReaderFailed }> {
  try {
    const result = await runReader(name, input, gateway);
    // runReader parsed it with the reader's answer schema.
    const answer = result.answer as A;
    const usd = usdOf(result.spent);
    writeWhole(
      out,
      json({
        ...entry,
        answer,
        ...after(answer),
        refusals: result.refusals,
        spent: result.spent,
        usd,
      })
    );
    return { answer, usd };
  } catch (error) {
    if (!(error instanceof ReaderFailed)) throw error;
    const { refusals, spent } = error;
    writeWhole(
      out,
      json({ ...entry, answer: null, ...after(null), refusals, spent, usd: usdOf(spent) })
    );
    return { answer: undefined, failed: error };
  }
}

/** One fresh request of the craft reader on `input`, the input of `skill`: writes its exact user
 *  message to `inputFile(out)` and the entry to `out`, whole. The entry names the skill (its
 *  directory, source and each file with its SHA-256), the style text (file, source, SHA-256 and
 *  how many rules), the craft list (file, source, SHA-256 and the reader properties sent), each
 *  nearest skill, the reader's prompt and settings, the answer (null when every reply was
 *  refused), the refusals and the spend. */
async function craftRead(
  reads: CraftReads,
  skill: ReadSkill,
  input: CraftInput,
  out: string,
  gateway: Gateway
): Promise<{ answer: CraftAnswer; usd: number } | { answer: undefined; failed: ReaderFailed }> {
  const message = inputFile(out);
  writeWhole(message, READERS.craft.render(input));
  const entry = {
    entry: "craft",
    skill: {
      name: skill.name,
      dir: skill.dir,
      source: reads.skillSource,
      files: fileDigests(skill.files),
    },
    ...rulesRecords(reads, input.properties),
    nearest: reads.nearest.map((each) => ({
      name: each.name,
      dir: each.dir,
      files: fileDigests(each.files),
    })),
    reader: { prompt: "craft", settings: loadPrompt("craft").settings },
    input: message,
  };
  return await readToFile<CraftAnswer>("craft", input, gateway, out, entry);
}

/** The gaps of `answer` in a few words: `no gap`, or their count and their rules. */
function gapWords(answer: CraftAnswer): string {
  if (answer.gaps.length === 0) return "no gap";
  const rules = [...new Set(answer.gaps.map((gap) => gap.rule))];
  return `${counted(answer.gaps.length, "gap")} (${rules.join(", ")})`;
}

/** `skill-cuts craft`. */
async function craftCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(argv, CRAFT_OPTIONS, CRAFT_USAGE);
  const given = required(values, CRAFT_REQUIRED, CRAFT_USAGE);
  const reads = craftReads(given, values);
  const input = craftReaderInput(reads);
  const gateway = gatewayOf(configFrom(given.config), given["key-file"], values, io.spendRecorder);
  const logged = loggedGateway(
    gateway,
    values["request-log"] ?? `${given.out}.${REQUEST_LOG}`,
    "reader:craft"
  );
  const read = await craftRead(reads, reads.skill, input, given.out, logged);
  if (read.answer === undefined) {
    io.err(`craft: ${read.failed.message}; written to ${given.out}`);
    return EXIT.failed;
  }
  io.out(
    `craft: ${reads.skill.name}: ${gapWords(read.answer)}, $${read.usd.toFixed(4)} at list price; written to ${given.out}\n`
  );
  return EXIT.ok;
}

const NEAREST_FLAGS =
  "--config <skill-cuts.json> --key-file <file> --skill <dir> [--skill-source <text>] --candidates <source>=<root>... --out <row.json>";
const NEAREST_USAGE = `usage: cli.ts nearest ${NEAREST_FLAGS} [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]`;

/** `skill-cuts nearest`: one fresh request of the nearest reader, on one skill and every skill of
 *  each `--candidates` root. */
async function nearestCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(
    argv,
    {
      ...GATEWAY_OPTIONS,
      skill: { type: "string" },
      "skill-source": { type: "string" },
      candidates: { type: "string", multiple: true },
      out: { type: "string" },
    },
    NEAREST_USAGE
  );
  const given = required(values, ["config", "key-file", "skill", "out"], NEAREST_USAGE);
  const roots = (values.candidates ?? []).map((flag) => {
    const [source, root] = pairOf("--candidates", flag, "<source>=<root>");
    return { source, root, names: skillDirs(root) };
  });
  if (roots.length === 0) throw new Refused(`--candidates required. ${NEAREST_USAGE}`);
  const skillMd = path.join(given.skill, "SKILL.md");
  if (!isFile(skillMd)) throw new Refused(`--skill ${given.skill} has no SKILL.md`);
  const text = ompText(readFileSync(skillMd));
  const frontmatter = readFrontmatter(text);
  const skill = {
    name: ompSkillName(frontmatter, path.basename(given.skill)),
    description: textOr(frontmatter.description, ""),
    // The text less its frontmatter, as Oh My Pi splits it (common.ts's `splitFrontmatter` with
    // `repair`: past any leading HTML comments, trimmed); a text with no block whole, less the
    // blank lines before it and the white space after it.
    body: splitFrontmatter(text, true).body.replace(/^\n+/, "").trimEnd(),
  };
  const candidates = roots.flatMap(({ source, root, names }) =>
    names.map((name) => {
      const read = readFrontmatter(ompText(readFileSync(path.join(root, name, "SKILL.md"))));
      return { name: ompSkillName(read, name), source, description: textOr(read.description, "") };
    })
  );
  const checked = READERS.nearest.input.safeParse({ skill, candidates });
  if (!checked.success) {
    throw new Refused(`the nearest reader's input is refused: ${schemaFaults(checked.error)}`);
  }
  const gateway = gatewayOf(configFrom(given.config), given["key-file"], values, io.spendRecorder);
  const logged = loggedGateway(
    gateway,
    values["request-log"] ?? `${given.out}.${REQUEST_LOG}`,
    "reader:nearest"
  );
  const message = inputFile(given.out);
  writeWhole(message, READERS.nearest.render(checked.data));
  const row = {
    row: "nearest",
    skill: {
      name: skill.name,
      description: skill.description,
      source: values["skill-source"] ?? null,
      dir: given.skill,
    },
    candidates: roots.map(({ source, root, names }) => ({ source, root, skills: names.length })),
    reader: { prompt: "nearest", settings: loadPrompt("nearest").settings },
    input: message,
  };
  const read = await readToFile<NearestAnswer>(
    "nearest",
    checked.data,
    logged,
    given.out,
    row,
    (answer) => ({
      nearest:
        answer === null
          ? []
          : answer.nearest.map((entry) => ({
              ...entry,
              description:
                candidates.find((each) => each.name === entry.name && each.source === entry.source)
                  ?.description ?? "",
            })),
    })
  );
  if (read.answer === undefined) {
    io.err(`nearest: ${read.failed.message}; written to ${given.out}`);
    return EXIT.failed;
  }
  const named = read.answer.nearest.map((entry) => `${entry.name} (${entry.source})`).join(", ");
  io.out(
    `nearest: ${skill.name}: ${read.answer.verdict}${named === "" ? "" : `: ${named}`}, $${read.usd.toFixed(4)} at list price; written to ${given.out}\n`
  );
  return EXIT.ok;
}

const COVERAGE_USAGE =
  "usage: cli.ts craft-coverage --root <dir> [--source <text>] [--skip <name>]... --entries <reader-findings.jsonl>";

/** What craft-coverage reads of a craft entry (`craftRead`): its skill's directory and source, and
 *  whether it has an answer, with its gaps: proposals.ts's FindingsEntry, each gap of any shape. */
const CoverageEntry = FindingsEntry.extend({
  answer: z.looseObject({ gaps: z.array(z.unknown()) }).nullable(),
});
type CoverageEntry = z.infer<typeof CoverageEntry>;

/** Each craft entry of the entries file `file` (a `reader-findings.jsonl`) with its line, as
 *  `schema` reads it, in the file's order, and the file's bytes; its other lines (a proposal's
 *  entry) are passed over. Throws a refusal naming the file when it cannot be read, or naming a
 *  line that is no JSON or that `schema` refuses. */
function craftEntries<T>(
  file: string,
  schema: z.ZodType<T>
): { bytes: Buffer; entries: { line: number; entry: T }[] } {
  const bytes = readBytes(file, "the entries");
  const text = bytes.toString("utf8");
  const entries = text.split("\n").flatMap((line, index) => {
    if (line.trim() === "") return [];
    const where = `line ${index + 1} of the entries ${file}`;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      throw new Refused(`${where} is not JSON`);
    }
    if (field(value, "entry") !== "craft") return [];
    const entry = schema.safeParse(value);
    if (!entry.success) {
      throw new Refused(`${where} is no craft entry: ${schemaFaults(entry.error)}`);
    }
    return [{ line: index + 1, entry: entry.data }];
  });
  return { bytes, entries };
}

/** `skill-cuts craft-coverage`: each skill of a root beside its craft entry, or none. */
function craftCoverageCommand(argv: readonly string[], io: CliIo): number {
  const values = parse(
    argv,
    {
      root: { type: "string" },
      source: { type: "string" },
      skip: { type: "string", multiple: true },
      entries: { type: "string" },
    },
    COVERAGE_USAGE
  );
  const given = required(values, ["root", "entries"], COVERAGE_USAGE);
  const skip = new Set(values.skip ?? []);
  const all = skillDirs(given.root);
  const names = all.filter((name) => !skip.has(name));
  const skipped = all.filter((name) => skip.has(name));
  // Each skill's last craft entry of the source, by its directory's name, and its count of them.
  const entries = new Map<string, { last: CoverageEntry; count: number }>();
  for (const { entry } of craftEntries(given.entries, CoverageEntry).entries) {
    if (values.source !== undefined && entry.skill.source !== values.source) continue;
    const name = path.basename(entry.skill.dir);
    entries.set(name, { last: entry, count: (entries.get(name)?.count ?? 0) + 1 });
  }
  const missing = names.filter((name) => !entries.has(name));
  const lines = names.map((name) => {
    const found = entries.get(name);
    if (found === undefined) return `${name}: none`;
    const { last, count } = found;
    const what =
      last.answer === null
        ? "every reply refused"
        : last.answer.gaps.length === 0
          ? "no gap"
          : counted(last.answer.gaps.length, "gap");
    return `${name}: entry: ${what}${count > 1 ? ` (the last of ${count} entries)` : ""}`;
  });
  const source = values.source === undefined ? "" : `, source ${values.source}`;
  const skippedWords = skipped.length === 0 ? "" : ` (skipped: ${skipped.join(", ")})`;
  io.out(
    `${[
      `craft-coverage: ${given.root}${source}: ${counted(names.length, "skill")}${skippedWords}, ${names.length - missing.length} with an entry`,
      ...lines,
    ].join("\n")}\n`
  );
  if (missing.length === 0) return EXIT.ok;
  io.err(
    `craft-coverage: ${counted(missing.length, "skill")} under ${given.root} ${missing.length === 1 ? "has" : "have"} no entry: ${missing.join(", ")}`
  );
  return EXIT.failed;
}

// `proposals`: the open proposals of closed pull requests (proposals.ts).

const PROPOSALS_FLAGS =
  "--config <skill-cuts.json> --key-file <file> --body <file> --body-source <text> --section <heading> --list <text>... --pr-diff <id prefix>=<file>... --skills-root <repo>=<dir>... --repo-root <repo>=<dir> --style <file> --style-source <text> --craft <craft-list.json> --craft-source <text> --findings <reader-findings.jsonl> --out <dir>";
const PROPOSALS_DRY_RUN = "--dry-run --body <file> --section <heading> --list <text>...";
const PROPOSALS_USAGE = [
  `usage: cli.ts proposals ${PROPOSALS_FLAGS} [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]`,
  `       cli.ts proposals ${PROPOSALS_DRY_RUN}`,
].join("\n");

/** `skill-cuts proposals`: each item of the named lists of a pull request's body, read with the
 *  proposal-target and proposal readers (proposals.ts); with `--dry-run`, the items only. */
async function proposalsCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(
    argv,
    {
      ...GATEWAY_OPTIONS,
      "dry-run": { type: "boolean" },
      body: { type: "string" },
      "body-source": { type: "string" },
      section: { type: "string" },
      list: { type: "string", multiple: true },
      "pr-diff": { type: "string", multiple: true },
      "skills-root": { type: "string", multiple: true },
      "repo-root": { type: "string" },
      style: { type: "string" },
      "style-source": { type: "string" },
      craft: { type: "string" },
      "craft-source": { type: "string" },
      findings: { type: "string" },
      out: { type: "string" },
    },
    PROPOSALS_USAGE
  );
  const lists = values.list ?? [];
  if (lists.length === 0) throw new Refused(`--list required. ${PROPOSALS_USAGE}`);
  const { body, section } = required(values, ["body", "section"], PROPOSALS_USAGE);
  const bodyBytes = readBytes(body, "the body");
  let items: ProposalItem[];
  try {
    items = parseProposalItems(bodyBytes.toString("utf8"), section, lists);
  } catch (error) {
    if (!(error instanceof ItemFaults)) throw error;
    io.err(`proposals: ${body}: ${error.message}`);
    return EXIT.failed;
  }
  if (values["dry-run"] === true) {
    const counts = lists.map(
      (list) => `${items.filter((item) => item.list === list).length} in ${JSON.stringify(list)}`
    );
    io.out(
      `${[
        `proposals: ${body}: ${counted(items.length, "item")}: ${counts.join(", ")}`,
        ...items.map((item) => `${item.id} (${item.state}): line ${item.line}`),
      ].join("\n")}\n`
    );
    return EXIT.ok;
  }
  const given = required(
    values,
    [
      ...["config", "key-file", "body-source", "repo-root", "style", "style-source", "craft"],
      ...["craft-source", "findings", "out"],
    ] as const,
    PROPOSALS_USAGE
  );
  const diffs = new Map<string, PullDiff>();
  for (const flag of values["pr-diff"] ?? []) {
    const [prefix, file] = pairOf("--pr-diff", flag, "<id prefix>=<file>");
    if (diffs.has(prefix)) throw new Refused(`--pr-diff gives the prefix ${prefix} twice`);
    const bytes = readBytes(file, "the diff");
    const text = bytes.toString("utf8");
    let files: DiffFile[];
    try {
      files = splitDiff(text);
    } catch (error) {
      throw new Refused(`the diff ${file} of --pr-diff ${prefix} is refused: ${messageFor(error)}`);
    }
    diffs.set(prefix, { pr: prefix, file, sha256: sha256(bytes), text, files });
  }
  const undiffed = [...new Set(items.map((item) => idPrefix(item.id)))].filter(
    (prefix) => !diffs.has(prefix)
  );
  if (undiffed.length > 0) {
    const named = undiffed.map((prefix) => {
      const ids = items.filter((item) => idPrefix(item.id) === prefix).map((item) => item.id);
      return `${prefix} (${ids.join(", ")})`;
    });
    throw new Refused(`no --pr-diff gives the diff of ${named.join("; ")}`);
  }
  const skillsRoots: { repo: string; dir: string; names: string[] }[] = [];
  for (const flag of values["skills-root"] ?? []) {
    const [repo, dir] = pairOf("--skills-root", flag, "<repo>=<dir>");
    if (skillsRoots.some((root) => root.repo === repo)) {
      throw new Refused(`--skills-root gives the repository ${repo} twice`);
    }
    skillsRoots.push({ repo, dir, names: skillDirs(dir) });
  }
  if (skillsRoots.length === 0) throw new Refused(`--skills-root required. ${PROPOSALS_USAGE}`);
  const [repo, dir] = pairOf("--repo-root", given["repo-root"], "<repo>=<dir>");
  if (!statSync(dir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Refused(`--repo-root ${given["repo-root"]}: ${dir} is no directory`);
  }
  const rules = rulesReads(given);
  const findings = craftEntries(given.findings, FindingsEntry);
  const run: ProposalsRun = {
    items,
    body: { file: body, source: given["body-source"], sha256: sha256(bodyBytes) },
    diffs,
    skillsRoots,
    repoRoot: { repo, dir },
    style: rules.style,
    craft: rules.craft,
    findings: { file: given.findings, sha256: sha256(findings.bytes), entries: findings.entries },
    out: given.out,
  };
  const pending = pendingItems(items, given.out);
  const gateway = gatewayOf(configFrom(given.config), given["key-file"], values, io.spendRecorder);
  const log = values["request-log"] ?? path.join(given.out, REQUEST_LOG);
  io.out(
    `proposals: ${counted(items.length, "item")}, ${pending.length} to read; the others have an entry under ${given.out}\n`
  );
  const missing = await runProposals(
    run,
    pending,
    {
      target: loggedGateway(gateway, log, "reader:proposal-target"),
      proposal: loggedGateway(gateway, log, "reader:proposal"),
    },
    io
  );
  const spent = `$${gateway.spend().usd.toFixed(4)} at list price`;
  if (missing.length > 0) {
    io.err(
      `proposals: ${counted(missing.length, "item")} of ${items.length} ${missing.length === 1 ? "has" : "have"} no entry: ${missing.join(", ")}; ${spent}`
    );
    return EXIT.failed;
  }
  io.out(`proposals: each of ${counted(items.length, "item")} has an entry; ${spent}\n`);
  return EXIT.ok;
}

const CONTROL_CRAFT_USAGE = `usage: cli.ts control craft (--remove-section <heading> | --join-lines <a>-<b>) --copy-line <a>[-<b>] [--copy-kind <present|missing>]... [--copy-rule <id>]... [--copy-claim <text>] --original-line <a>[-<b>|all] [--original-kind <present|missing>]... [--original-rule <id>]... [--original-claim <text>] ${CRAFT_FLAGS} --out <dir> [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]`;

const HEADING_LINE = /^(#{1,6})\s/;
const LIST_ITEM = /^\s*(?:[-*+]|\d+[.)])\s+/;

/** `lines` as the text of a file, with a final newline where `text` had one. */
function withLines(text: string, lines: readonly string[]): string {
  return `${lines.join("\n")}${text.endsWith("\n") ? "\n" : ""}`;
}

/** `text`, a SKILL.md, with the section `heading` removed: the heading's line and every line up
 *  to the next heading of its level or a higher one. A line in a fenced block is no heading
 *  (proposals.ts's `fencedLines`). Throws a refusal when no line, or more than one, is that
 *  heading. */
function removeSection(text: string, heading: string): string {
  const lines = textLines(text);
  const inFence = fencedLines(lines);
  const at = lines.flatMap((line, index) =>
    !inFence[index] && line.trim() === heading.trim() ? [index] : []
  );
  if (at.length !== 1) {
    throw new Refused(
      at.length === 0
        ? `no line of SKILL.md is the heading ${heading}`
        : `lines ${at.map((index) => index + 1).join(", ")} of SKILL.md are each the heading ${heading}`
    );
  }
  const start = at[0] ?? 0;
  const level = HEADING_LINE.exec(lines[start] ?? "")?.[1]?.length;
  if (level === undefined) throw new Refused(`--remove-section ${heading} is no markdown heading`);
  let end = start + 1;
  for (; end < lines.length; end++) {
    const depth = inFence[end] ? undefined : HEADING_LINE.exec(lines[end] ?? "")?.[1]?.length;
    if (depth !== undefined && depth <= level) break;
  }
  return withLines(text, [...lines.slice(0, start), ...lines.slice(end)]);
}

/** `text`, a SKILL.md, with the list items of lines `from` to `to` joined into one paragraph at
 *  line `from`, their list markers removed. Throws a refusal when a line of them is no list item
 *  or the range runs past the file's end. */
function joinLines(text: string, from: number, to: number): string {
  const lines = textLines(text);
  if (to > lines.length) {
    throw new Refused(
      `--join-lines ${from}-${to} runs past the last line, ${lines.length}, of SKILL.md`
    );
  }
  for (let line = from; line <= to; line++) {
    if (!LIST_ITEM.test(lines[line - 1] ?? ""))
      throw new Refused(`line ${line} of SKILL.md is no list item`);
  }
  const joined = lines
    .slice(from - 1, to)
    .map((line) => line.replace(LIST_ITEM, "").trim())
    .join(" ");
  return withLines(text, [...lines.slice(0, from - 1), joined, ...lines.slice(to)]);
}

/** One side of a control craft, the copy or the unchanged skill, and its criteria: a gap is a
 *  candidate when its line is in `lines` (of SKILL.md; `all`: any line of any file) and, where
 *  given, its kind is one of `kinds` and its rule one of `rules`. With a `claim`, the claim reader
 *  answers whether each candidate states it. */
interface Side {
  readonly name: "copy" | "original";
  readonly lines: { readonly from: number; readonly to: number } | "all";
  readonly kinds: readonly CraftGap["kind"][];
  readonly rules: readonly string[];
  readonly claim: string | undefined;
}

/** Each side of a control craft, as the report's words name it. */
const SIDE_WORDS: Readonly<Record<Side["name"], string>> = {
  copy: "the copy",
  original: "the unchanged skill",
};

/** A candidate of a side: its gap, and the claim reader's answer (null where the side gives no
 *  claim; `states` null where every reply was refused), with the file it is written to. */
interface Candidate {
  readonly gap: CraftGap;
  readonly claim: { states: boolean | null; reason: string; file: string } | null;
}

/** The side `name` of a control craft, as its flags give it (`--<name>-line`, `-kind`, `-rule` and
 *  `-claim`). Throws a refusal on a line range, a kind or a rule it cannot read, and on a claim of
 *  more than one line. */
function sideOf(
  name: Side["name"],
  given: {
    readonly line: string;
    readonly kinds: readonly string[] | undefined;
    readonly rules: readonly string[] | undefined;
    readonly claim: string | undefined;
  },
  reads: CraftReads,
  input: CraftInput
): Side {
  const flag = `--${name}`;
  const range = given.line;
  const parts = /^(\d+)(?:-(\d+))?$/.exec(range);
  const from = Number(parts?.[1]);
  const to = Number(parts?.[2] ?? parts?.[1]);
  if (name === "copy" ? parts === null : parts === null && range !== "all") {
    throw new Refused(`${flag}-line ${range} is not <a>[-<b>]${name === "copy" ? "" : " or all"}`);
  }
  if (parts !== null && (from < 1 || to < from)) {
    throw new Refused(`${flag}-line ${range} is not a range of lines from 1`);
  }
  const kinds = (given.kinds ?? []).map((kind) => {
    const parsed = CraftGap.shape.kind.safeParse(kind);
    if (!parsed.success) throw new Refused(`${flag}-kind ${kind} is not present or missing`);
    return parsed.data;
  });
  const rules = given.rules ?? [];
  const ids = new Set([...input.style, ...input.properties].map((rule) => rule.id));
  for (const rule of rules) {
    if (!ids.has(rule) && !NEAREST_RULE.test(rule)) {
      throw new Refused(
        `${flag}-rule ${rule} is no rule of the style text ${reads.style.file}, no reader property of the craft list and no nearest:<skill>:<path>:<line>`
      );
    }
  }
  const claim = given.claim;
  if (claim !== undefined && !OneLine.safeParse(claim).success) {
    throw new Refused(`${flag}-claim is not one line`);
  }
  return { name, lines: parts === null ? "all" : { from, to }, kinds, rules, claim };
}

/** `side`'s criteria in words. */
function sideWords(side: Side): string {
  const lines =
    side.lines === "all"
      ? "any line"
      : side.lines.from === side.lines.to
        ? `line ${side.lines.from}`
        : `lines ${side.lines.from}-${side.lines.to}`;
  const kinds = side.kinds.length === 0 ? "any kind" : side.kinds.join(" or ");
  const rules = side.rules.length === 0 ? "any rule" : side.rules.join(" or ");
  const claim = side.claim === undefined ? "no claim" : `claim: ${side.claim}`;
  return `${lines}, ${kinds}, ${rules}, ${claim}`;
}

/** The candidates of `side` among the gaps of `answer` (the craft answer on `skill`), each with
 *  the claim reader's answer where the side gives a claim (one fresh request a candidate, its
 *  input and answer written under `<out>/claims/`); whether the side holds, and why. The copy
 *  holds when a candidate states its claim (any candidate, with no claim); the unchanged skill
 *  when none does (no candidate, with no claim), and a candidate whose every claim reply was
 *  refused does not let it hold. */
async function judgeSide(
  side: Side,
  answer: CraftAnswer,
  skill: ReadSkill,
  out: string,
  gateway: Gateway
): Promise<{ candidates: Candidate[]; holds: boolean; why: string }> {
  const gaps = answer.gaps.filter(
    (gap) =>
      (side.lines === "all" ||
        (gap.file === "SKILL.md" && gap.line >= side.lines.from && gap.line <= side.lines.to)) &&
      (side.kinds.length === 0 || side.kinds.includes(gap.kind)) &&
      (side.rules.length === 0 || side.rules.includes(gap.rule))
  );
  const claim = side.claim;
  const candidates = await Promise.all(
    gaps.map(async (gap, index): Promise<Candidate> => {
      if (claim === undefined) return { gap, claim: null };
      const file = path.join(out, "claims", `${side.name}-${index + 1}.json`);
      const input = ClaimInput.parse({ claim, gap, files: skill.files });
      writeWhole(inputFile(file), READERS.claim.render(input));
      const read = await readToFile<ClaimAnswer>("claim", input, gateway, file, {
        side: side.name,
        candidate: index + 1,
        input,
        prompt: "claim",
        settings: loadPrompt("claim").settings,
      });
      return read.answer === undefined
        ? { gap, claim: { states: null, reason: read.failed.message, file } }
        : { gap, claim: { ...read.answer, file } };
    })
  );
  const places = (some: readonly Candidate[]) =>
    some.map((each) => `${each.gap.file}:${each.gap.line}`).join(", ");
  const words = SIDE_WORDS[side.name];
  if (candidates.length === 0) {
    return { candidates, holds: side.name === "original", why: `${words} has no candidate` };
  }
  if (claim === undefined) {
    return {
      candidates,
      holds: side.name === "copy",
      why: `${words} has a candidate: ${places(candidates)}`,
    };
  }
  const stating = candidates.filter((each) => each.claim?.states === true);
  const unknown = candidates.filter((each) => each.claim?.states === null);
  if (stating.length > 0) {
    return {
      candidates,
      holds: side.name === "copy",
      why: `a candidate of ${words} states the claim: ${places(stating)}`,
    };
  }
  if (side.name === "original" && unknown.length > 0) {
    return {
      candidates,
      holds: false,
      why: `the claim reader's every reply was refused for ${places(unknown)} of ${words}`,
    };
  }
  return {
    candidates,
    holds: side.name === "original",
    why: `no candidate of ${words} states the claim (${counted(candidates.length, "candidate")})`,
  };
}

/** A candidate as a line of the report: its place, kind, rule and why, and its claim answer. */
function candidateLine(candidate: Candidate): string {
  const { gap, claim } = candidate;
  const said =
    claim === null
      ? ""
      : claim.states === null
        ? ` (${claim.reason})`
        : ` (${claim.states ? "states" : "does not state"} the claim: ${claim.reason})`;
  return `  ${gap.file}:${gap.line} ${gap.kind} ${gap.rule}: ${gap.why}${said}`;
}

/** `skill-cuts control craft`: the craft reader's control (README.md, "control craft"). */
async function controlCraftCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(
    argv,
    {
      ...CRAFT_OPTIONS,
      "remove-section": { type: "string" },
      "join-lines": { type: "string" },
      "copy-line": { type: "string" },
      "copy-kind": { type: "string", multiple: true },
      "copy-rule": { type: "string", multiple: true },
      "copy-claim": { type: "string" },
      "original-line": { type: "string" },
      "original-kind": { type: "string", multiple: true },
      "original-rule": { type: "string", multiple: true },
      "original-claim": { type: "string" },
    },
    CONTROL_CRAFT_USAGE
  );
  const given = required(
    values,
    [...CRAFT_REQUIRED, "copy-line", "original-line"],
    CONTROL_CRAFT_USAGE
  );
  const removed = values["remove-section"];
  const joined = values["join-lines"];
  if ((removed === undefined) === (joined === undefined)) {
    throw new Refused(`give one of --remove-section and --join-lines. ${CONTROL_CRAFT_USAGE}`);
  }
  const reads = craftReads(given, values);
  const originalInput = craftReaderInput(reads);
  const copySide = sideOf(
    "copy",
    {
      line: given["copy-line"],
      kinds: values["copy-kind"],
      rules: values["copy-rule"],
      claim: values["copy-claim"],
    },
    reads,
    originalInput
  );
  const originalSide = sideOf(
    "original",
    {
      line: given["original-line"],
      kinds: values["original-kind"],
      rules: values["original-rule"],
      claim: values["original-claim"],
    },
    reads,
    originalInput
  );
  const skillText = reads.skill.files[0]?.text ?? "";
  let changed: string;
  if (removed !== undefined) changed = removeSection(skillText, removed);
  else {
    const range = /^(\d+)-(\d+)$/.exec(joined ?? "");
    const [from, to] = [Number(range?.[1]), Number(range?.[2])];
    if (range === null || from < 1 || to <= from) {
      throw new Refused(`--join-lines ${joined} is not <a>-<b>, two lines from 1 with a before b`);
    }
    changed = joinLines(skillText, from, to);
  }
  const copies = path.join(given.out, "copy");
  if (existsSync(copies)) {
    throw new Refused(`--out ${given.out} holds a copy already: give a fresh --out`);
  }
  const gateway = gatewayOf(configFrom(given.config), given["key-file"], values, io.spendRecorder);
  const log = values["request-log"] ?? path.join(given.out, REQUEST_LOG);
  const copyDir = path.join(copies, path.basename(path.resolve(given.skill)));
  // The directory a link names, so that the change is never written through it into the skill.
  cpSync(realpathSync(given.skill), copyDir, { recursive: true });
  writeWhole(path.join(copyDir, "SKILL.md"), changed);
  const copy = readSkill(copyDir, "the copy");
  const craftGateway = loggedGateway(gateway, log, "reader:craft");
  const files = {
    copy: path.join(given.out, "copy.json"),
    original: path.join(given.out, "original.json"),
  };
  const [copyRead, originalRead] = await Promise.all([
    craftRead(reads, copy, craftReaderInput(reads, copy), files.copy, craftGateway),
    craftRead(reads, reads.skill, originalInput, files.original, craftGateway),
  ]);
  const change = removed !== undefined ? { removeSection: removed } : { joinLines: joined ?? "" };
  const record = (side: Side, skill: ReadSkill, entry: string) => ({
    skill: skill.dir,
    entry,
    lines: side.lines === "all" ? "all" : `${side.lines.from}-${side.lines.to}`,
    kinds: side.kinds,
    rules: side.rules,
    claim: side.claim ?? null,
  });
  const report = [
    `control craft: ${reads.skill.name}: ${removed !== undefined ? `--remove-section ${removed}` : `--join-lines ${joined}`}; the copy is ${copyDir}`,
  ];
  let control: Record<string, unknown>;
  let held: boolean;
  let why: string;
  if (copyRead.answer === undefined || originalRead.answer === undefined) {
    held = false;
    why = [
      ...(copyRead.answer === undefined
        ? [`on ${SIDE_WORDS.copy}, ${copyRead.failed.message}`]
        : []),
      ...(originalRead.answer === undefined
        ? [`on ${SIDE_WORDS.original}, ${originalRead.failed.message}`]
        : []),
    ].join("; ");
    control = {
      copy: record(copySide, copy, files.copy),
      original: record(originalSide, reads.skill, files.original),
    };
  } else {
    const claimGateway = loggedGateway(gateway, log, "reader:claim");
    const [copyJudged, originalJudged] = await Promise.all([
      judgeSide(copySide, copyRead.answer, copy, given.out, claimGateway),
      judgeSide(originalSide, originalRead.answer, reads.skill, given.out, claimGateway),
    ]);
    held = copyJudged.holds && originalJudged.holds;
    why = `${copyJudged.why}; ${originalJudged.why}`;
    control = {
      copy: { ...record(copySide, copy, files.copy), ...copyJudged },
      original: { ...record(originalSide, reads.skill, files.original), ...originalJudged },
    };
    for (const [side, judged] of [
      [copySide, copyJudged],
      [originalSide, originalJudged],
    ] as const) {
      report.push(
        `${side.name} (${sideWords(side)}): ${counted(judged.candidates.length, "candidate")}; ${judged.holds ? "holds" : "does not hold"}: ${judged.why}`,
        ...judged.candidates.map(candidateLine)
      );
    }
  }
  const usd = gateway.spend().usd;
  writeWhole(
    path.join(given.out, "control.json"),
    json({ control: "craft", skill: given.skill, change, ...control, held, why, usd })
  );
  io.out(`${report.join("\n")}\n`);
  const line = `control craft: ${held ? "held" : "not held"}: ${why}; $${usd.toFixed(4)} at list price; written to ${given.out}`;
  if (held) io.out(`${line}\n`);
  else io.err(line);
  return held ? EXIT.ok : EXIT.failed;
}

// `behaviour`: the results of a case.

/** Where the deployment that answered a saved run's reply comes from (`behaviour
 *  continue-mixed`): the run's own record (its last `deployments` entry), or, for a run saved
 *  before runs recorded one, its saved `--out`'s request log, the one deployment every logged
 *  request of its session id was answered by, with the log's path and SHA-256. */
const SavedDeployment = z.union([
  z.strictObject({ id: z.string(), from: z.literal("run") }),
  z.strictObject({
    id: z.string(),
    from: z.literal("request log"),
    file: z.string(),
    sha256: z.string(),
  }),
]);
type SavedDeployment = z.infer<typeof SavedDeployment>;

/** What `behaviour continue-mixed` made of the saved run in a run's place: `continued`, the run
 *  is its continuation from its saved reply; or `run again`, the saved run gets no verdict and the
 *  stage's extra-run rule runs it again fresh (the run's reason says why). `saved` names the saved
 *  run (its number in its arm and stage, its blind id, its round, its verdict, and the runner
 *  that made it); `runner` is the runner that continued it (`runnerDigest`); `deployment` the
 *  deployment that answered its saved reply and where that came from, or null where none was
 *  taken; `requestIds` the provider request id of each continuation request that got an answer,
 *  in the order sent (null where an answer carried none); and `usd` the dollars at list price of
 *  the continuation's answered requests and of its judgement. */
const ContinueMixedRun = z.strictObject({
  kind: z.enum(["continued", "run again"]),
  saved: z.strictObject({
    run: z.int().positive(),
    id: z.string().nullable(),
    round: z.int().positive(),
    verdict: z.enum(["pass", "fail"]).nullable(),
    runner: z.string(),
  }),
  runner: z.string(),
  deployment: SavedDeployment.nullable(),
  requestIds: z.array(z.string().nullable()),
  usd: z.number(),
});
type ContinueMixedRun = z.infer<typeof ContinueMixedRun>;

/** One run of one arm of a case in one stage: score.ts's RunRecord, and what the run did. */
const Run = z.strictObject({
  ...RunRecord.shape,
  /** The run's number in its arm and stage, from 1. */
  run: z.int().positive(),
  /** The round of the stage that started it, from 1; its blind order's seed, and its send
   *  order's (replay.ts's `sendOrder`), is `seeds[round - 1]`. */
  round: z.int().positive(),
  /** Its place in that round's send order, from 0. */
  sendPlace: z.int().nonnegative(),
  /** The session id every request of the run went with (its `metadata.user_id` or
   *  `prompt_cache_key`), its own, or null for a body that names none. */
  userId: z.string().nullable(),
  /** The routing id every request of the run went with, in gateway.ts's ROUTING_HEADER: the one
   *  of its round, the runs numbered `run` of every arm of its stage in one stage command (a
   *  random UUID, drawn once per case, stage command and run number), or its arm's `--route-id`
   *  pin; null for a Responses body, which goes with none; absent from a run made before this
   *  field. */
  routingId: z.string().nullable().optional(),
  /** True where `routingId` is its arm's `--route-id` pin, a diagnostic run; absent otherwise. */
  routePinned: z.literal(true).optional(),
  /** Its reply's opaque id in that round's blind order, or null when it was not judged. */
  id: z.string().nullable(),
  /** The rounds of skill reads the replay answered before the step, or null when it failed. */
  skillRounds: z.int().nonnegative().nullable(),
  /** The rounds of read-only calls a written case's replay answered from its checkout copy
   *  before the step, or null when it failed; absent from a run made before this field. */
  checkoutRounds: z.int().nonnegative().nullable().optional(),
  /** Each answered round's answers, in order (a skill read's, replay.ts's AnsweredRead, or a
   *  checkout read's, its CheckoutRead), or null when the replay failed; absent from a run made
   *  before this field. */
  skillReads: z
    .array(z.array(z.union([AnsweredRead, CheckoutRead])))
    .nullable()
    .optional(),
  /** Why the reply that ended the run, every call of it read-only, was not answered from the
   *  checkout copy, in a run made before such a run got no verdict (its reason now begins
   *  UNANSWERED_READ); absent otherwise. */
  unanswered: z.string().optional(),
  /** The calls each continued round left out (replay.ts's LeftOutCall: a reply that read skill
   *  files beside calls that only prepare the case's act, whose reads were answered and sent back
   *  alone), in order, or null when the replay failed; absent from a run made before this
   *  field. */
  leftOut: z.array(LeftOutCall).nullable().optional(),
  /** True where the reply its step ends on read skill files beside an act (replay.ts's
   *  `replyBoundary`: `acts`), so it was judged at a mixed reply; absent otherwise. */
  mixedAct: z.literal(true).optional(),
  /** The step's content (each answered round's answered calls, then the reply after them, which
   *  for a run that read past the last round, made a call the checkout answers in no form, or
   *  whose mixed reply was undecided, is the reply it ended on, as it came), or null when the
   *  replay failed. */
  reply: z.array(z.looseObject({ type: z.string() })).nullable(),
  /** Whole replies of earlier answered rounds, when saved for a written case. */
  answeredReplies: z.array(z.array(z.looseObject({ type: z.string() }))).optional(),
  /** What the run's replay and its judgement used, each call's usage and dollars. A replay that
   *  failed (`REPLAY_FAILED`) records none, though the rounds it had answered were paid. */
  replaySpent: z.array(SpentRecord),
  /** The deployment that answered each of the replay's requests, in the order sent (gateway.ts's
   *  `deploymentOf`: the first 8 hex digits of its `x-litellm-model-id`, "" where the answer
   *  carried none); empty when the replay failed, as `replaySpent` is; absent from a run made
   *  before this field. */
  deployments: z.array(z.string()).optional(),
  judgeSpent: z.array(SpentRecord),
  /** The runner that made the run (`runnerDigest`); absent from a run made before runs recorded
   *  it, which takes its stage's (`stageRunners`). */
  runner: z.string().optional(),
  /** What `behaviour continue-mixed` made of the saved run in this run's place
   *  (ContinueMixedRun); absent on every other run. */
  continueMixed: ContinueMixedRun.optional(),
});
type Run = z.infer<typeof Run>;

/** How a case's session is taken to have had Dispatch, which the Envoy plugin's block rests on
 *  (arms.ts's `dispatchOf`): from a Legion role, from its transcript, or on the assumption. */
const DispatchFrom = z.enum(["legion-role", "transcript", "assumed"]);
type DispatchFrom = z.infer<typeof DispatchFrom>;

/** The capture an arm's runs replay: its body file, below `--out`, and the two digests; and, for
 *  a capture that carries the Envoy plugin's block of the skill, where the answer that the
 *  session had Dispatch comes from (absent otherwise, and in a stage made before this field). */
const CaptureNote = z.strictObject({
  bodyFile: z.string(),
  captureDigest: z.string().nullable(),
  sessionDigest: z.string().nullable(),
  dispatch: DispatchFrom.optional(),
});

/** How a stage's runs were sent, as each stage result records it: each run with a session id of
 *  its own, and every arm's runs of a round in one seeded order. A stage result that does not
 *  record it was made before, when an arm's runs shared its capture's one id and went as one
 *  batch, so its runs are no runs of this procedure. */
export const SENDS = "each run its own session id; a round's arms interleaved in one seeded order";

/** What a stage whose arms hold the arm of ours records of it: our skill's name; whether our text
 *  replaced a skill of our name that the case's profile held, which makes the arm a bundle (their
 *  skill hidden, ours at the given text); and the path inside the skill of each history read of
 *  theirs it removed because ours has no file there (arms.ts's `Arm.ours`). */
const OursNote = z.strictObject({
  name: z.string().min(1),
  replacedOwn: z.boolean(),
  removedReads: z.array(z.string()),
});
type OursNote = z.infer<typeof OursNote>;

/** The digests (`textDigest`) of the skill directories a run reads: `--old`, and `--new` or
 *  `--ours` where given; each stage records those it ran with. */
const Texts = z.strictObject({
  old: z.string(),
  new: z.string().optional(),
  ours: z.string().optional(),
});
type Texts = z.infer<typeof Texts>;

/** A written case's shared checkout as a stage records it (`StageResult.checkout`) and its marker
 *  begins (`CheckoutMarker`). `jj`, once a case whose session ran jj needed it: true when it was
 *  made a colocated jj repository (checkout.ts's `colocateJj`), else why it could not be. */
const SharedCheckout = z.strictObject({
  git: z.boolean(),
  depth: z.int().positive().nullable(),
  removedLinks: z.array(z.string()),
  jj: z.union([z.literal(true), z.strictObject({ why: z.string() })]).optional(),
});

/** The shell a written case's stage answered its `bash` calls in (`caseShell`, checkout.ts's
 *  ShellSetup): `prefix`, the PI_SHELL_PREFIX its context session's Oh My Pi ran them with (null
 *  for none); and, where the profile's dotfiles commit holds a bash gate, `gate`, its experiments
 *  extension's bash gate: the dotfiles commit it was loaded at, whether that version was taken
 *  from the profile's commit (no refusal of the gate in the context session's history: `profile`)
 *  or from those refusals (`history`; checkout.ts's `gateVersion`), and the arm (`leading`); else
 *  `interceptor`, Oh My Pi's own interceptor as that commit's omp/config.yml sets it: the commit,
 *  and whether it is on (`enabled`; checkout.ts's `interceptorOf`), a form a stage recorded before
 *  it was does not hold; or why the runner does not know it, and no `bash` call of the stage is
 *  answered. */
const StageShell = z.union([
  z.strictObject({
    prefix: z.string().nullable(),
    gate: z.strictObject({
      commit: z.string(),
      from: z.enum(["history", "profile"]),
      leading: z.boolean(),
    }),
  }),
  z.strictObject({
    prefix: z.string().nullable(),
    interceptor: z.strictObject({ commit: z.string(), enabled: z.boolean() }),
  }),
  z.strictObject({ why: z.string() }),
]);
type StageShell = z.infer<typeof StageShell>;

/** One stage of a case: `running` until score.ts's `stageOutcome` says `counted` or `unscreened`.
 *  `texts` are the digests of `--old` and of `--new` or `--ours` it ran with, `compared` the arm
 *  the old text was compared with (a stage that records none was run with `--new`, before it was
 *  recorded), `ours` the arm of ours' note where its arms hold it, and `plantedLine` the line
 *  `minus` left out in place of the case's (control only). `judge` is the judge's identity
 *  (judge.ts's JudgeIdentity) when the stage was made, before its first judgement; a stage made
 *  before it was recorded has none. `sends` is SENDS. `usd` is what its calls cost at list price:
 *  the gateway's spend while the stage ran, in each process that ran it, so its runs' replays and
 *  judgements and the calls no run records (its captures' hint calls, the rounds a replay that
 *  then failed had answered, a reply whose judgement the budget refused); never the calls of a
 *  round stopped before it was saved. `runner` is the runner that started it (`runnerDigest`),
 *  absent in a stage started before the runner was recorded; each of its runs records its own.
 *  `confirmFlagged` is true in a confirm that `behaviour confirm --confirm-flagged` ran, in any
 *  of its rounds, and absent otherwise: report then gives no skill verdict
 *  (CONFIRM_FLAGGED_VERDICT). `checkout`, in a written case's stage, is the shared checkout its
 *  read-only calls were answered from (`sharedCheckout`): whether it is a git checkout (else
 *  GitHub's tarball, whose git commands are not answered), the depth of its history (`git log`
 *  there shows at most that many commits, then the shallow boundary; null for the tarball), each
 *  symbolic link removed from it because it resolved outside it or not at all (checkout.ts's
 *  `removeOutsideLinks`), whether it is a colocated jj repository (`jj`, where a case whose
 *  session ran jj made it one or failed to), and the shell its `bash` calls were answered in
 *  (`shell`, StageShell; absent in a stage recorded before it was); absent otherwise.
 *  `continued` records the source `--out`, the number of saved rounds, and whether
 *  continue-mixed selected the stage. Later runs without a ContinueMixedRun are fresh runs. */
const StageResult = z.strictObject({
  state: StageState,
  sends: z.literal(SENDS),
  texts: Texts,
  compared: Compared.optional(),
  ours: OursNote.optional(),
  plantedLine: z.string().nullable(),
  judge: JudgeIdentity.optional(),
  captures: z.record(z.string(), CaptureNote),
  seeds: z.array(z.string()),
  runs: z.array(Run),
  usd: z.number(),
  runner: z.string().optional(),
  confirmFlagged: z.literal(true).optional(),
  checkout: SharedCheckout.extend({ shell: StageShell.optional() }).optional(),
  continued: z
    .strictObject({
      from: z.string(),
      savedRounds: z.int().nonnegative(),
      selected: z.boolean(),
    })
    .optional(),
});
type StageResult = z.infer<typeof StageResult>;

/** The compared arm `stage` records, `new` for a stage of the cut's runner that records none. */
function recordedArm(stage: StageResult): Compared {
  return stage.compared ?? "new";
}

const Judgement = z.strictObject({
  verdict: z.enum(["pass", "fail"]).nullable(),
  reason: z.string(),
});

/** How the drop of a case whose block of its skill the arms cannot each rebuild begins. */
const INJECTION_UNREBUILDABLE = "injection-unrebuildable";
/** The kinds of such a block: arms.ts's `injectionFaults` (a `/skill:` prompt the user typed, a
 *  block with arguments or in another form, a dispatch-first block away from where the plugin
 *  puts it), and capture.ts's InjectionRefused (a subagent whose agent autoloads the skill and
 *  whose messages hold no block of it). */
const InjectionKind = z.enum([
  "user-invoked",
  "other-shape",
  "plugin-block-misplaced",
  "autoload-no-block",
]);
type InjectionKind = z.infer<typeof InjectionKind>;

/** How the drop of a case begins whose file, read whole by path, an arm's text holds in a form
 *  the arms do not rebuild (`pathFormDrop`); then the arm, then the fault. */
const PATH_FORM = "path-form";

/** Everything the runner keeps of one case: why it was dropped, if it was, and the stage that
 *  dropped it (`droppedAt`: `screen` for a drop by the judge controls, the model, the listing, a
 *  block its arms cannot rebuild or a file read by path an arm's text holds in a form the arms do
 *  not rebuild (`path-form`), else the stage whose capture needed a directory no read gives
 *  or found such a block, or whose runs left it unscreened (`reads-past-last-round`,
 *  `provider-refused`); absent when it was not dropped, and in a result written before the
 *  stage was recorded, `dropStage`); for an `injection-unrebuildable` drop, the kind of each fault
 *  (`injectionKinds`); its judge controls; each stage it ran, `confirm` its latest confirm and
 *  `earlierConfirms` the finished ones before it, oldest first; and the peak resident memory at
 *  the end of its last stage. */
export const CaseResult = z.strictObject({
  caseId: z.string().min(1),
  skill: z.string().min(1),
  cut: z.string().min(1),
  dropped: z.string().nullable(),
  droppedAt: Stage.optional(),
  controls: z
    .strictObject({
      realStep: Judgement,
      violating: Judgement,
      nothing: Judgement,
      failed: z.array(ControlName),
      hold: z.boolean(),
      usd: z.number(),
    })
    .nullable(),
  stages: z.strictObject({
    screen: StageResult.optional(),
    confirm: StageResult.optional(),
    control: StageResult.optional(),
  }),
  earlierConfirms: z.array(StageResult).optional(),
  injectionKinds: z.array(InjectionKind).optional(),
  peakRssBytes: z.number().nullable(),
});
export type CaseResult = z.infer<typeof CaseResult>;

/** The file of case `caseId`'s results under `out`. */
export function caseResultFile(out: string, caseId: string): string {
  return path.join(out, "cases", `${encodeURIComponent(caseId)}.json`);
}

/** A digest of `files`, each its name and its path: each name and its bytes, in name order. */
function filesDigest(files: readonly (readonly [name: string, file: string])[]): string {
  const hash = createHash("sha256");
  for (const [name, file] of [...files].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
    hash.update(name).update("\0").update(readFileSync(file)).update("\0");
  }
  return hash.digest("hex").slice(0, 16);
}

/** The files of the directory `dir` that `keep` keeps, each its path inside `dir` and its path. */
function directoryFiles(dir: string, keep: (file: string) => boolean): [string, string][] {
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((file) => keep(file) && statSync(path.join(dir, file)).isFile())
    .map((file) => [file, path.join(dir, file)]);
}

/** A digest of the directory `dir`: each file's path inside it and its bytes, in path order, of
 *  the files `keep` keeps (every file by default). */
export function textDigest(dir: string, keep: (file: string) => boolean = () => true): string {
  return filesDigest(directoryFiles(dir, keep));
}

/** `texts` as score.ts's MadeConfirm holds them: the old text's digest and the compared arm's in
 *  one string, `none` for no skill. */
function textsKey(texts: Texts): string {
  return `${texts.old} ${texts.new ?? texts.ours ?? "none"}`;
}

/** The modules of ../skill-scenarios that the TypeScript files `files` of this directory import,
 *  and those modules' own imports of that directory, followed until none is new: each by its path
 *  from this directory, and its path. */
function scenarioImports(files: readonly string[]): [string, string][] {
  const scenarios = path.join(import.meta.dir, "..", "skill-scenarios");
  const found = new Map<string, string>();
  const follow = (file: string) => {
    for (const [, spec = ""] of readFileSync(file, "utf8").matchAll(/\bfrom\s+"(\.\.?\/[^"]+)"/g)) {
      const resolved = path.resolve(path.dirname(file), spec);
      const module = resolved.endsWith(".ts") ? resolved : `${resolved}.ts`;
      if (!module.startsWith(`${scenarios}${path.sep}`) || found.has(module)) continue;
      found.set(module, path.relative(import.meta.dir, module));
      follow(module);
    }
  };
  for (const file of files) if (file.endsWith(".ts")) follow(file);
  return [...found].map(([module, name]) => [name, module]);
}

/** The runner's own code, as each stage it starts and each run it makes records it: a digest of
 *  this directory less its tests (`*.test.ts`), its test data and README.md, and of each module of
 *  ../skill-scenarios its files import (`scenarioImports`, those modules' own imports of that
 *  directory too). Two confirms of one case are pooled only when one runner made both
 *  (score.ts's `confirmScore`), so a confirm made before a fix of the runner is not pooled with
 *  one made after it; a stage command takes no saved stage of another runner as its own
 *  (`checkRunners`), and report reports no results of more than one runner unless told to
 *  (`runnerCounts`). */
export function runnerDigest(): string {
  const own = directoryFiles(
    import.meta.dir,
    (file) =>
      !file.endsWith(".test.ts") && file !== "README.md" && file.split(path.sep)[0] !== "testdata"
  );
  return filesDigest([...own, ...scenarioImports(own.map(([, file]) => file))]);
}

/** The runner of a stage made before stages recorded theirs, and of its runs, as the report and
 *  the stage commands name it and as `--allow-runners` names it. */
const UNRECORDED_RUNNER = "unrecorded";

/** Each runner that made runs of `stage`, with how many it made: a run's own `runner`, else (a run
 *  made before runs recorded it) its stage's, else UNRECORDED_RUNNER. A stage with no run counts
 *  under its own runner, with 0 runs. */
function stageRunners(stage: StageResult): Map<string, number> {
  const own = stage.runner ?? UNRECORDED_RUNNER;
  const runs = new Map<string, number>();
  for (const run of stage.runs) {
    const runner = run.runner ?? own;
    runs.set(runner, (runs.get(runner) ?? 0) + 1);
  }
  if (runs.size === 0) runs.set(own, 0);
  return runs;
}

/** Where the captures of `arm` are saved under `out`: one directory per arm and text. `texts` are
 *  the digests of `--old`, `--new` and `--ours` (`textDigest`), so only `minus`'s copy is hashed
 *  here; `twin` holds the old text. */
export function armCaptureDir(out: string, arm: Arm, texts: Texts): string {
  const dir = (text: string) => path.join(out, "captures", `${arm.name}-${text}`);
  if (arm.skillDir === undefined) return dir("none");
  if (arm.name === "old" || arm.name === "new" || arm.name === "ours") {
    return dir(texts[arm.name] ?? textDigest(arm.skillDir));
  }
  if (arm.name === "twin") return dir(texts.old);
  return dir(textDigest(arm.skillDir));
}

/** An estimate of the chance that the registered rule calls a loss in one informative case when
 *  the compared arm's text is the old text: score.ts's `lossChance` with both texts passing at p =
 *  (old + compared + 1) / (2 × SCREEN_RUNS + 2), the case's old and compared arm's screen passes
 *  pooled with a Laplace term, and no skill at q = (none + 1) / (SCREEN_RUNS + 2); with no skill
 *  compared, no skill is the compared arm, at p. */
export function falseLossChance(
  passes: { old: number; compared: number; none: number },
  compared: Compared = "new"
): number {
  const p = (passes.old + passes.compared + 1) / (2 * SCREEN_RUNS + 2);
  const q = (passes.none + 1) / (SCREEN_RUNS + 2);
  return lossChance({ old: p, compared: p, none: compared === "none" ? p : q }, compared);
}

/** The report's sensitivity table: the old text's pass rates, the drops of the cut's text from
 *  them, and the no-skill pass rate. */
const SENSITIVITY = { old: [0.9, 0.7], drops: [0.1, 0.2, 0.3, 0.4], none: 0.1 } as const;

// `behaviour`: running the stages.

/** A behaviour command's cases and where it writes. `compared` is the arm the old text is
 *  compared with (`report`'s: the one its results record). `servedModels` are the configuration's,
 *  `readingModels` the skill's reading sessions by model that `--reading-models` names, with its
 *  file, and `limits` the text `--limits` names, with its file (each undefined without its flag).
 *  `allowRunners` are the runners `--allow-runners` names, whose saved stages a stage command
 *  takes as this runner's, and whose results report reports with others' (none without it).
 *  `storeOnly` is the skill's counted and store-only sessions that `--store-only` names (report
 *  only; undefined without it). */
interface Behaviour {
  readonly io: CliIo;
  readonly command: string;
  readonly out: string;
  readonly casesFile: string;
  readonly cases: readonly Case[];
  readonly compared: Compared;
  /** Whether a result made with another compared arm than `compared` is refused (`readResult`):
   *  for a stage command, and for a report given `--new` or `--ours`; a report given neither
   *  takes the arm its results record, and `audit-mixed` skips each stage run with other texts
   *  than its own. */
  readonly checkCompared: boolean;
  readonly servedModels: readonly string[];
  readonly readingModels: { readonly file: string; readonly count: ModelCount } | undefined;
  readonly limits: { readonly file: string; readonly text: string } | undefined;
  readonly allowRunners: readonly string[];
  readonly storeOnly: StoreOnly | undefined;
}

/** What a stage command runs with. `gateway` counts the spend; `replays`, `judge` and the capture
 *  send through it, each logged. `source` reads the context's repositories, and counts its GitHub
 *  reads; `cacheDir` holds the context's builds, among them each installed skill a replay answers
 *  a read of another skill from (context.ts's `installedCopies`). `runner` is this runner's code
 *  (`runnerDigest`), which each stage it starts and each run it makes records; `again` the cases
 *  `--again` names, and `confirmFlagged` whether `--confirm-flagged` is given (both confirm
 *  only); `routePins` the routing id `--route-id` pins each arm it names to (none without it). */
interface Runner extends Behaviour {
  readonly dirs: ArmDirs;
  readonly texts: Texts;
  readonly scratch: string;
  readonly cacheDir: string;
  readonly gateway: Gateway;
  readonly replays: Gateway;
  readonly judge: Gateway;
  readonly source: RepoSource;
  /** Where a written case's repository is fetched from (`sharedCheckout`): GitHub's
   *  `https://github.com/<repo>.git`, or, with `--repos <dir>`, the git repository
   *  `<dir>/<repo>.git`. */
  readonly gitRemote: (repo: string) => string;
  /** The configuration's `dotfilesRepo`, the dev box's dotfiles repository, whose commit a
   *  written case's profile names gives the shell its `bash` calls are answered in (`caseShell`);
   *  undefined where it names none. */
  readonly dotfilesRepo: string | undefined;
  capture(c: Case, arm: Arm): Promise<Capture>;
  readonly afterCase: string | undefined;
  readonly afterCaseFailures: string[];
  readonly runner: string;
  readonly again: readonly string[];
  readonly confirmFlagged: boolean;
  readonly routePins: ReadonlyMap<ArmName, string>;
}

/** The skill names the judge reads as `the-skill` in every judgement of case `c` (blind.ts): in a
 *  run with an arm of ours, the case's skill and ours, so the judge cannot tell old from ours by
 *  a name; none in a run with `--new` (both arms hold the case's skill) or with no skill compared
 *  (the judge reads the case's own history in every arm), whose judge reads the case's text as
 *  it is. */
function blinded(r: Runner, c: Case): string[] {
  return r.dirs.ours === undefined ? [] : [c.skill, r.dirs.ours.name];
}

/** The milliseconds of the phases of one case's stage, from `startedAt` (`performance.now()`), for
 *  its line (`phaseWords`): its judge controls, each arm's capture, the context builds within them
 *  and the GitHub reads made in those (how many, and the milliseconds one ran), its replays, its
 *  judgements, and its result writes. */
interface Phases {
  readonly startedAt: number;
  controlsMs: number;
  readonly captureMs: Partial<Record<ArmName, number>>;
  contextMs: number;
  github: { readonly calls: number; readonly ms: number };
  replaysMs: number;
  judgeMs: number;
  writeMs: number;
}

/** The phases of a case's stage that starts now, none of them run yet. */
function newPhases(): Phases {
  return {
    startedAt: performance.now(),
    controlsMs: 0,
    captureMs: {},
    contextMs: 0,
    github: { calls: 0, ms: 0 },
    replaysMs: 0,
    judgeMs: 0,
    writeMs: 0,
  };
}

/** The seconds of `p`, as a case's line ends them: each phase, the rest, and the total so far. */
function phaseWords(p: Phases): string {
  const s = (ms: number) => (ms / 1000).toFixed(1);
  const total = performance.now() - p.startedAt;
  const arms = Object.entries(p.captureMs);
  const captures = arms.reduce((sum, [, ms]) => sum + ms, 0);
  const named = p.controlsMs + captures + p.replaysMs + p.judgeMs + p.writeMs;
  const each = arms.map(([arm, ms]) => `${arm} ${s(ms)}`).join(", ") || "none";
  return `seconds: judge controls ${s(p.controlsMs)}, captures ${s(captures)} (${each}; context ${s(p.contextMs)}; ${p.github.calls} GitHub reads, ${s(p.github.ms)}), replays ${s(p.replaysMs)}, judge ${s(p.judgeMs)}, write ${s(p.writeMs)}, other ${s(Math.max(0, total - named))}, total ${s(total)}`;
}

/** Writes one line of the command's progress. */
function say(b: Behaviour, line: string): void {
  b.io.out(`${b.command}: ${line}\n`);
}

/** Case `caseId`'s results under `b.out`, or undefined when it has none. Refuses a result one of
 *  whose stages was run before each run had its own session id and the arms were interleaved
 *  (its stage records no `sends`, SENDS), and, where `b.checkCompared`, one of whose stages was
 *  run with another compared arm than `b.compared`. */
function readResult(b: Behaviour, caseId: string): CaseResult | undefined {
  const file = caseResultFile(b.out, caseId);
  if (!existsSync(file)) return undefined;
  const raw = readJson(file, "the case result");
  const stages: unknown = typeof raw === "object" && raw !== null ? Reflect.get(raw, "stages") : {};
  const before = Stage.options.filter((stage) => {
    const state: unknown =
      typeof stages === "object" && stages !== null ? Reflect.get(stages, stage) : undefined;
    return typeof state === "object" && state !== null && !Object.hasOwn(state, "sends");
  });
  if (before.length > 0) {
    throw new Refused(
      `${file} holds ${before.join(" and ")} runs sent before each run had its own session id and a round's arms were interleaved, so they are no runs of this procedure: use a new --out`
    );
  }
  const parsed = CaseResult.safeParse(raw);
  if (!parsed.success) {
    throw new Refused(`${file} is not a case result: ${schemaFaults(parsed.error)}`);
  }
  for (const stage of b.checkCompared ? Stage.options : []) {
    const state = parsed.data.stages[stage];
    if (state !== undefined && recordedArm(state) !== b.compared) {
      throw new Refused(
        `${file} holds runs made with the compared arm ${recordedArm(state)}, not ${b.compared}, this command's: give the --new or --ours they were made with (neither for none), or another --out`
      );
    }
  }
  return parsed.data;
}

/** The results of each case of `b` that has some, by case id. */
function readResults(b: Behaviour): Map<string, CaseResult> {
  const results = new Map<string, CaseResult>();
  for (const c of b.cases) {
    const result = readResult(b, c.id);
    if (result !== undefined) results.set(c.id, result);
  }
  return results;
}

/** `result` as the score reads it (`scored`): each run of each stage, earlier confirms included,
 *  with no reply, no reads and no left-out calls, which the score does not read, so that the
 *  results kept for the score hold no reply's content. */
function forScore(result: CaseResult): CaseResult {
  const slim = (stage: StageResult): StageResult => ({
    ...stage,
    runs: stage.runs.map(({ answeredReplies: _answeredReplies, ...run }) => ({
      ...run,
      reply: null,
      skillReads: null,
      leftOut: null,
    })),
  });
  const { screen, confirm, control } = result.stages;
  return {
    ...result,
    stages: {
      ...(screen === undefined ? {} : { screen: slim(screen) }),
      ...(confirm === undefined ? {} : { confirm: slim(confirm) }),
      ...(control === undefined ? {} : { control: slim(control) }),
    },
    ...(result.earlierConfirms === undefined
      ? {}
      : { earlierConfirms: result.earlierConfirms.map(slim) }),
  };
}

/** Writes `result` whole, its time counted in `phases` when given. Returns its file. */
function writeResult(b: Behaviour, result: CaseResult, phases?: Phases): string {
  const started = performance.now();
  const file = caseResultFile(b.out, result.caseId);
  writeWhole(file, json(result));
  if (phases !== undefined) phases.writeMs += performance.now() - started;
  return file;
}

/** The screen and control run records of `results`, as score.ts reads them, a dropped case's
 *  included: score.ts reads its drop from its facts (`caseFacts`), and each confirm from there. */
function runRecords(results: Iterable<CaseResult>): RunRecord[] {
  const records: RunRecord[] = [];
  for (const result of results) {
    for (const stage of ["screen", "control"] as const) {
      for (const { caseId, arm, verdict, reason } of result.stages[stage]?.runs ?? []) {
        records.push({ caseId, arm, stage, verdict, reason });
      }
    }
  }
  return records;
}

/** The served-sessions refusal of `b`'s skill (score.ts's `servedScreeningRefusal`, with
 *  `--reading-models`), which stops its session cases only: a case runs only on a served model, and
 *  a written case is a written task on a served model, not a session. Undefined without
 *  `--reading-models` or with enough reading sessions on served models. */
function sessionRefusal(b: Behaviour): NoVerdict | undefined {
  return b.readingModels === undefined ? undefined : servedScreeningRefusal(b.readingModels.count);
}

/** The served-sessions refusal (`sessionRefusal`) as the whole skill's: a skill with written cases
 *  is screened on them, so only one with none gets no verdict by it. */
function skillRefusal(b: Behaviour): NoVerdict | undefined {
  return b.cases.some((c) => caseKind(c) === "written") ? undefined : sessionRefusal(b);
}

/** The kind of each case of `b` by its id (cases.ts's `caseKind`). */
function kindsOf(b: Behaviour): (caseId: string) => CaseKind {
  const kinds = new Map(b.cases.map((c) => [c.id, caseKind(c)]));
  return (caseId) => kinds.get(caseId) ?? "session";
}

/** The run records of `b`'s `results` that its score counts (`runRecords`): while the
 *  served-sessions refusal applies (`sessionRefusal`), none of a session case whose screen is not
 *  counted or unscreened, since the screen takes no session case then (screenCommand) and a stage
 *  still running would keep the verdict waiting for ever. */
function scoredRecords(b: Behaviour, results: ReadonlyMap<string, CaseResult>): RunRecord[] {
  if (sessionRefusal(b) === undefined) return runRecords(results.values());
  const kindOf = kindsOf(b);
  return runRecords(
    [...results.values()].filter((result) => {
      const state = result.stages.screen?.state;
      return kindOf(result.caseId) !== "session" || state === "counted" || state === "unscreened";
    })
  );
}

/** The stage that dropped `result`: its `droppedAt`, or for a result written before that was
 *  recorded, the stage its runs show: screen when its screen runs are not counted, else confirm
 *  when its screen flagged it (by the compared arm the screen records) and it has no confirm runs,
 *  else control. */
function dropStage(result: CaseResult): Stage {
  if (result.droppedAt !== undefined) return result.droppedAt;
  const screen = result.stages.screen;
  const compared = screen === undefined ? "new" : recordedArm(screen);
  const outcome =
    screen === undefined ? undefined : stageOutcome(screen.runs, result.caseId, "screen", compared);
  if (outcome?.state !== "counted") return "screen";
  return isFlagged(outcome.passes, compared) && result.stages.confirm === undefined
    ? "confirm"
    : "control";
}

/** Each confirm of `result`, oldest first: its earlier ones, then its latest. */
function confirmStages(result: CaseResult): StageResult[] {
  const latest = result.stages.confirm;
  return [...(result.earlierConfirms ?? []), ...(latest === undefined ? [] : [latest])];
}

/** Where one stage of a case result is: its kind, its number among the case's confirms (from 1,
 *  oldest first; confirms only), how it is read from a result, and a result with a new state of it
 *  in its place. */
interface StagePlace {
  readonly stage: Stage;
  readonly confirm?: number;
  get(result: CaseResult): StageResult | undefined;
  put(result: CaseResult, state: StageResult): CaseResult;
}

/** The place of the latest stage of kind `stage`, `result.stages[stage]`. */
function latestPlace(stage: Stage, confirm?: number): StagePlace {
  return {
    stage,
    ...(confirm === undefined ? {} : { confirm }),
    get: (result) => result.stages[stage],
    put: (result, state) => ({ ...result, stages: { ...result.stages, [stage]: state } }),
  };
}

/** The place of each stage `result` ran, in the order screen, each confirm oldest first (its
 *  earlier ones, then its latest, numbered from 1), control. */
function stagePlaces(result: CaseResult): StagePlace[] {
  const earlier = result.earlierConfirms ?? [];
  const { screen, confirm, control } = result.stages;
  return [
    ...(screen === undefined ? [] : [latestPlace("screen")]),
    ...earlier.map(
      (_, at): StagePlace => ({
        stage: "confirm",
        confirm: at + 1,
        get: (each) => each.earlierConfirms?.[at],
        put: (each, state) => ({
          ...each,
          earlierConfirms: (each.earlierConfirms ?? []).map((kept, k) => (k === at ? state : kept)),
        }),
      })
    ),
    ...(confirm === undefined ? [] : [latestPlace("confirm", earlier.length + 1)]),
    ...(control === undefined ? [] : [latestPlace("control")]),
  ];
}

/** A stage's place in words: `screen`, `confirm 2`, `control`. */
function placeWords(place: Pick<StagePlace, "stage" | "confirm">): string {
  return place.confirm === undefined ? place.stage : `${place.stage} ${place.confirm}`;
}

/** The drop at screen of case `c` for each block of its skill its arms cannot each rebuild
 *  (arms.ts's `injectionFaults`), or undefined when it holds none: its reason names each fault's
 *  kind, then each fault. */
function injectionDrop(c: Case): { reason: string; kinds: InjectionKind[] } | undefined {
  const faults = injectionFaults(c);
  if (faults.length === 0) return undefined;
  const kinds = [...new Set(faults.map((fault) => fault.kind))];
  const texts = faults.map((fault) => fault.text).join("; ");
  return { reason: `${INJECTION_UNREBUILDABLE}: ${kinds.join(", ")}: ${texts}`, kinds };
}

/** The drop at screen of case `c` when an arm's text of `dirs` (old, new, ours) holds a file the
 *  case reads whole by path from its copy (`pathCopy`, cases.ts's `pathReadFile`) in a form the
 *  arms do not rebuild (build.ts's `pathFormIn`: arms.ts's `pathFormFault`, or a directory), on
 *  which buildArm would throw at the capture; undefined when it holds none. Its reason is
 *  `path-form: <arm>: <fault>`. */
function pathFormDrop(c: Case, dirs: ArmDirs): string | undefined {
  if (c.pathCopy === undefined) return undefined;
  const targets = readTargets(c.messages);
  const files = c.skillReads.flatMap((index) => {
    const message = c.messages[index];
    const target = targets.get(message?.toolCallId ?? "") ?? "";
    if (message?.role !== "toolResult") return [];
    const file = pathReadFile(c, target);
    return file === undefined ? [] : [file];
  });
  const found = pathFormIn(files, { old: dirs.old, new: dirs.new, ours: dirs.ours?.dir });
  return found === undefined ? undefined : `${PATH_FORM}: ${found.arm}: ${found.fault}`;
}

/** The drop score.ts reads of case `c`, whose result is `result` (undefined when it has none):
 *  the drop its result records (`dropStage`), else its listing measure's (score.ts's
 *  `listingStanding`), else its block its arms cannot rebuild (`injectionDrop`), at screen; or
 *  undefined when it has none. */
function caseDrop(c: Case, result: CaseResult | undefined): CaseDrop | undefined {
  if (result !== undefined && result.dropped !== null) {
    return { stage: dropStage(result), reason: result.dropped };
  }
  const listing = listingStanding(c.evidence);
  if (listing.state === "dropped") return { stage: "screen", reason: listing.reason };
  const injection = injectionDrop(c);
  return injection === undefined ? undefined : { stage: "screen", reason: injection.reason };
}

/** What score.ts reads of `b`'s cases beside their run records (CaseFacts): the drop of each case
 *  that has one (`caseDrop`), and each case's confirms with the runner and texts of each, old
 *  against `b.compared`. */
function caseFacts(b: Behaviour, results: ReadonlyMap<string, CaseResult>): Map<string, CaseFacts> {
  const facts = new Map<string, CaseFacts>();
  for (const c of b.cases) {
    const result = results.get(c.id);
    const drop = caseDrop(c, result);
    const confirms: MadeConfirm[] = (result === undefined ? [] : confirmStages(result)).map(
      (stage) => ({
        runner: stage.runner ?? null,
        texts: textsKey(stage.texts),
        outcome: stageOutcome(stage.runs, c.id, "confirm", b.compared),
      })
    );
    facts.set(c.id, { ...(drop === undefined ? {} : { drop }), confirms });
  }
  return facts;
}

/** The score of `b`'s skill from `results`, the old text compared with `b.compared` (score.ts's
 *  `scoreSkill`, of `scoredRecords` with `caseFacts`). */
function scored(b: Behaviour, results: ReadonlyMap<string, CaseResult>): SkillScore {
  return scoreSkill(scoredRecords(b, results), caseFacts(b, results), b.compared);
}

/** Why the screen of `b`'s skill is not finished, or undefined when it is: each case is dropped
 *  or has its screen counted or unscreened, or the screen takes it no more: a session case the
 *  served-sessions refusal stops (`sessionRefusal`), whether or not its screen began, as
 *  screenCommand passes it over; or a case of a kind whose screening stopped (score.ts's
 *  `screeningDone`) before it. */
function screenUnfinished(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string | undefined {
  const counts = screenCounts(score, kindsOf(b));
  const refused = sessionRefusal(b) !== undefined;
  const dropped = new Set(
    score.cases.flatMap((each) => (each.drop === undefined ? [] : [each.caseId]))
  );
  for (const c of screenOrder(b.cases, caseKind)) {
    if (dropped.has(c.id)) continue;
    const result = results.get(c.id);
    const state = result?.stages.screen?.state;
    if (state === "counted" || state === "unscreened") continue;
    const kind = caseKind(c);
    if ((kind === "session" && refused) || (result === undefined && screeningDone(counts, kind)))
      continue;
    return `case ${c.id} is not screened yet`;
  }
  return undefined;
}

/** The results of `b`'s cases that `keep` names of the skill's score, and the score, once its
 *  screen is finished: the other cases' results are read for the score only. Throws a refusal
 *  before that. */
function screened(
  b: Behaviour,
  keep: (score: SkillScore) => readonly (string | undefined)[]
): { results: Map<string, CaseResult>; score: SkillScore } {
  const results = readResults(b);
  const score = scored(b, results);
  const unfinished = screenUnfinished(b, results, score);
  if (unfinished !== undefined) {
    throw new Refused(`the screen is not finished: ${unfinished}; run behaviour screen first`);
  }
  const kept = new Set(keep(score));
  for (const id of results.keys()) if (!kept.has(id)) results.delete(id);
  return { results, score };
}

/** Why a skill whose screen is finished can get no verdict, whatever its confirm runs show, or
 *  undefined: too few informative cases; no informative case left to be the control case, each
 *  one dropped; or a control whose runs are made and whose planted-loss control is no loss or
 *  whose false-loss control is a loss (score.ts's `skillVerdict`, and its reason). Exported for
 *  cli.test.ts. */
export function noVerdict(score: SkillScore): string | undefined {
  const control = score.control;
  const counted = control?.state === "counted" ? control.passes : undefined;
  const controlLoss =
    control === undefined ||
    control.state === "running" ||
    (counted !== undefined && isControlLoss(counted));
  const falseLoss = counted !== undefined && isFalseLoss(counted);
  const outcome = skillVerdict({
    informative: score.informative,
    controlLoss,
    falseLoss,
    losses: 0,
  });
  if (outcome.verdict !== null) return score.noControl;
  return reasonWords(outcome.reason, control, null);
}

/** A reason of score.ts's (`skillVerdict`) as the runner says it. Where the planted-loss
 *  control's runs are unscreened, its reason (NO_CONTROL_LOSS) says so, since no deletion was
 *  measured; else it names the line `minus` left out: `plantedLine` where one was given. */
function reasonWords(
  reason: string,
  control: StageOutcome<"control"> | undefined,
  plantedLine: string | null
): string {
  if (control?.state === "unscreened") {
    return reason.replace(
      NO_CONTROL_LOSS,
      "planted-loss control not a loss: its runs are unscreened"
    );
  }
  return plantedLine === null
    ? reason
    : reason.replace(
        NO_CONTROL_LOSS,
        noControlLoss(`the planted line ${JSON.stringify(plantedLine)}`)
      );
}

/** The drop of a case whose model the gateway does not serve, as its drop begins and the report
 *  counts it. */
const MODEL_NOT_SERVED = "model-not-served";

/** The model of `servedModels` (the configuration's, each `provider/model`) that serves `model`:
 *  the one whose id, its text after its last `/`, is `model`'s; or undefined when none is. */
function servedModel(model: string, servedModels: readonly string[]): string | undefined {
  return servedModels.find((served) => modelId(served) === modelId(model));
}

/** Why the gateway runs nothing on the case model `model`: no model of `servedModels` (the
 *  configuration's) serves it. */
function notServedWords(model: string, servedModels: readonly string[]): string {
  return `its model ${model} is none the gateway serves (the configuration's servedModels: ${servedModels.join(", ") || "none"})`;
}

/** The directory under `--out` that keeps each session head a capture read (capture.ts's
 *  `sessionHeads`), one file per store and path hash. */
const SESSIONS_DIR = "sessions";

/**
 * What a capture of case `c` shows of the session it stands for (capture.ts's `sessionFacts`):
 * the session's head, read from its store (`c.source`) through the configuration's
 * `psqlCommand` once and saved under `<out>/sessions/`; the reminder Oh My Pi journaled for the
 * case's first message that goes to the model as a user turn (any but an assistant message or a
 * tool result), the turn a capture's reminder goes on, else one rendered for the UTC date of the
 * case's step (of its session's start when the step has no time). Throws when the
 * configuration names no such store or the head cannot be read: no capture of a session the
 * store holds is made with neutral values in place of a read that failed.
 */
async function caseSession(config: SkillCutsConfig, out: string, c: Case): Promise<SessionFacts> {
  const store = storeNamed(config, c.source.store).name;
  const heads = await sessionHeads(
    config.psqlCommand,
    store,
    [c.source.pathHash],
    path.join(out, SESSIONS_DIR)
  );
  const first = c.messages.find(
    (message) => message.role !== "assistant" && message.role !== "toolResult"
  );
  const turnAt: unknown = first?.timestamp;
  const attribution: unknown = first?.attribution;
  const stepAt: unknown = c.realStep.timestamp;
  return sessionFacts(heads.get(c.source.pathHash), {
    turn:
      typeof turnAt === "number"
        ? userTurnKey({
            timestamp: turnAt,
            attribution: typeof attribution === "string" ? attribution : undefined,
          })
        : undefined,
    date:
      typeof stepAt === "number"
        ? {
            value: new Date(stepAt).toISOString().slice(0, 10),
            from: "the UTC date of the case's step in the session",
          }
        : {
            value: new Date(c.sessionStart).toISOString().slice(0, 10),
            from: "the UTC date of the session's start (the case's step has no time)",
          },
    devbox: c.context === "devbox",
    devboxClaudeConfigDir: config.devboxClaudeConfigDir,
  });
}

/** A fresh result of case `c`. */
function newResult(c: Case): CaseResult {
  return {
    caseId: c.id,
    skill: c.skill,
    cut: c.cut,
    dropped: null,
    controls: null,
    stages: {},
    peakRssBytes: null,
  };
}

/** The texts of `texts` in words: `old <digest>`, then `new <digest>` or `ours <digest>`. */
function textWords(texts: Texts): string {
  return [
    `old ${texts.old}`,
    ...(texts.new === undefined ? [] : [`new ${texts.new}`]),
    ...(texts.ours === undefined ? [] : [`ours ${texts.ours}`]),
  ].join(", ");
}

/** Refuses a stage of case `caseId` that was run with other skill texts or another planted line
 *  than this run's: its runs and this run's would be of different arms. */
function checkSameTexts(r: Runner, caseId: string, stage: Stage, existing: StageResult): void {
  const plantedLine = stage === "control" ? (r.dirs.plantedLine ?? null) : null;
  if (
    existing.texts.old === r.texts.old &&
    existing.texts.new === r.texts.new &&
    existing.texts.ours === r.texts.ours &&
    existing.plantedLine === plantedLine
  ) {
    return;
  }
  throw new Refused(
    `case ${caseId}'s ${stage} runs in ${caseResultFile(r.out, caseId)} were made with other skill texts (${textWords(existing.texts)}, planted line ${JSON.stringify(existing.plantedLine)}) than this run's (${textWords(r.texts)}, planted line ${JSON.stringify(plantedLine)}): use the same --old, --new or --ours and --planted-line, or another --out`
  );
}

/** Whether case `result`'s `stage` is complete, refusing one made with other texts. */
function stageDone(r: Runner, result: CaseResult | undefined, stage: Stage): boolean {
  const existing = result?.stages[stage];
  if (result === undefined || existing === undefined) return false;
  checkSameTexts(r, result.caseId, stage, existing);
  return existing.state !== "running";
}

/** Refuses, before any model call, the saved `stage` of each of `results` some run of which (or
 *  which, with no run) a runner made that is neither this runner (`r.runner`) nor one
 *  `--allow-runners` names (`stageRunners`): a stage command takes a saved stage as done, or goes
 *  on with one still running, only when this runner made it or the user named its runner, so a
 *  stage of another runner is never skipped and never made again with no word. The refusal names
 *  each such case, its runners, and this runner, and advises an `--allow-runners` that names the
 *  runners it names already and those, since a new value replaces the old one. */
function checkRunners(r: Runner, results: Iterable<CaseResult>, stage: Stage): void {
  refuseOtherRunners(
    r,
    r.out,
    [...results].flatMap((result) => {
      const state = result.stages[stage];
      return state === undefined ? [] : [{ caseId: result.caseId, label: stage, state }];
    }),
    "a stage command takes a saved stage as done, or goes on with it,",
    "to take those stages as they are, or use another --out"
  );
}

/** The refusal of `checkRunners` and of `behaviour continue-mixed`, over `stages` (each a stage of
 *  case `caseId`'s result under `out`, named `label`): refuses each some run of which (or which,
 *  with no run) a runner made that is neither `r.runner` nor one `--allow-runners` names, naming
 *  each, its runners, this runner and the rule (`rule`, what takes such a stage), and advising an
 *  `--allow-runners` that names the runners it names already and those, `advice` after it. */
function refuseOtherRunners(
  r: Pick<Runner, "runner" | "allowRunners">,
  out: string,
  stages: readonly { caseId: string; label: string; state: StageResult }[],
  rule: string,
  advice: string
): void {
  const others = new Set<string>();
  const foreign = stages.flatMap(({ caseId, label, state }) => {
    const theirs = [...stageRunners(state).keys()].filter(
      (runner) => runner !== r.runner && !r.allowRunners.includes(runner)
    );
    for (const runner of theirs) others.add(runner);
    return theirs.length === 0
      ? []
      : [
          `case ${caseId}'s ${label} in ${caseResultFile(out, caseId)} was made by runner ${theirs.join(" and ")}`,
        ];
  });
  if (foreign.length === 0) return;
  const allow = new Set([...r.allowRunners, ...others]);
  throw new Refused(
    `${foreign.join("; ")}, and this runner is ${r.runner}: ${rule} only when this runner made it or --allow-runners names its runner. Give --allow-runners ${[...allow].join(",")} ${advice}`
  );
}

/** The judge controls of case `c`, kept in `result`, which is dropped when they do not hold: its
 *  drop names each control it failed. */
async function judgeTheControls(r: Runner, c: Case, result: CaseResult): Promise<CaseResult> {
  const before = r.gateway.spend().usd;
  const controls = await judgeControls(c, r.judge, blinded(r, c));
  const judged = (j: Judged) => ({ verdict: j.verdict, reason: j.reason });
  const next: CaseResult = {
    ...result,
    controls: {
      realStep: judged(controls.realStep),
      violating: judged(controls.violating),
      nothing: judged(controls.nothing),
      failed: controls.failed,
      hold: controls.hold,
      usd: r.gateway.spend().usd - before,
    },
  };
  if (controls.hold) return next;
  const failures = controls.failed.map(
    (name) =>
      `${CONTROLS[name].step} is judged ${controls[name].verdict ?? "not at all"} (${CONTROLS[name].wanted} wanted)`
  );
  return {
    ...next,
    dropped: `the judge controls do not hold: ${failures.join("; ")}`,
    droppedAt: "screen",
  };
}

/** How the reason of a run whose replay failed begins (runRound): `<REPLAY_FAILED>: <why>`. */
const REPLAY_FAILED = "the replay failed";

/** Round `round` of a stage: `toStart` replays of each arm, every arm's sent in one order under
 *  `seed` (replay.ts's `replayInterleaved`; the gateway bounds the calls in flight), each run with
 *  a session id of its own, the routing id `routingOf` gives its arm and its run number (its
 *  arm's runs so far, then its place among its arm's runs in the order sent), and its arm's
 *  installed copies of the profile's other skills, from its capture's record and the cache
 *  (context.ts's `installedCopies`), its mixed replies sorted by the case's act (replay.ts's
 *  `caseAct`), and, for a written case, `checkout`, the copy of its checkout its read-only calls
 *  are answered from, or why it has none (NO_CHECKOUT); then every reply judged blind under
 *  `seed`. A replay that fails is a run with no verdict, its reason `<REPLAY_FAILED>: <why>`
 *  (REFUSED_REPLAY reads a content-policy refusal there), and so is one whose reply after the
 *  last answered round still only reads, or reads skill files beside calls that only prepare
 *  (replay.ts's ReadsPastLastRound: its reason begins READS_PAST_LAST_ROUND), one whose mixed
 *  reply is undecided (UndecidedReply: its reason begins MIXED_UNDECIDED), and one that makes a
 *  read-only call the checkout answers in no form (UnansweredRead: its reason begins
 *  UNANSWERED_READ), each of which keeps that run's reads, left-out calls, reply, spend and
 *  deployments; one the budget refused, or whose judgement the budget refused, is no run. The
 *  runs are numbered, within their arm, in the order they were sent, and each records this
 *  runner (`r.runner`), its routing id, whether that is its arm's `--route-id` pin, the
 *  deployment that answered each of its requests, the calls its continued rounds left out, and
 *  whether it was judged at a mixed reply with an act (`mixedAct`). */
async function runRound(
  r: Runner,
  c: Case,
  stage: Stage,
  arms: ReadonlyMap<ArmName, { arm: Arm; capture: Capture }>,
  toStart: Partial<Record<ArmName, number>>,
  made: readonly Run[],
  round: number,
  seed: string,
  routingOf: (arm: ArmName, run: number) => string,
  phases: Phases,
  checkout: CheckoutCopy | Unanswered | undefined
): Promise<{ runs: Run[]; budgetSpent: boolean }> {
  const replaysStarted = performance.now();
  const sent = await replayInterleaved(
    [...arms].map(([name, { arm, capture }]) => {
      const runs = toStart[name] ?? 0;
      // The arm's runs in the order sent take the numbers after its runs so far.
      const before = made.filter((each) => each.arm === name).length;
      return {
        arm,
        body: capture.body,
        headers: capture.record.requestHeaders,
        copies: installedCopies(capture.record.context, r.cacheDir),
        runs,
        routingIds: Array.from({ length: runs }, (_, k) => routingOf(name, before + k + 1)),
      };
    }),
    r.replays,
    seed,
    {
      mixed: { kind: "case", act: caseAct(c) },
      ...(checkout === undefined ? {} : { checkout }),
    }
  );
  // Each reply is judged under its arm and its place among its arm's replays of this round.
  const replies: ArmReply[] = sent.flatMap(({ arm, run, outcome }) =>
    outcome.status === "fulfilled"
      ? [{ arm, run, content: outcome.value.step.message.content }]
      : []
  );
  const judgeStarted = performance.now();
  phases.replaysMs += judgeStarted - replaysStarted;
  const names = blinded(r, c);
  const judged =
    replies.length === 0 ? [] : (await judgeBlind(c, replies, r.judge, seed, names)).judged;
  phases.judgeMs += performance.now() - judgeStarted;
  const judgements = new Map(judged.map((j) => [`${j.arm}:${j.run}`, j]));
  let budgetSpent = false;
  const runs: Run[] = [];
  const numbered = new Map<ArmName, number>();
  for (const { arm, run: inArm, place: sendPlace, userId, routingId, outcome } of sent) {
    const base = {
      caseId: c.id,
      arm,
      stage,
      round,
      sendPlace,
      userId,
      routingId,
      ...(routingId !== null && r.routePins.has(arm) ? { routePinned: true as const } : {}),
      runner: r.runner,
    };
    const next = () => {
      const run = (numbered.get(arm) ?? made.filter((each) => each.arm === arm).length) + 1;
      numbered.set(arm, run);
      return run;
    };
    if (outcome.status === "rejected") {
      if (outcome.reason instanceof BudgetSpent) {
        budgetSpent = true;
        continue;
      }
      // A run whose reply after the last answered round still reads, whose mixed reply is
      // undecided, or that makes a read-only call the checkout answers in no form, keeps what it
      // read, left out and spent.
      const past =
        outcome.reason instanceof ReadsPastLastRound ||
        outcome.reason instanceof UndecidedReply ||
        outcome.reason instanceof UnansweredRead
          ? outcome.reason
          : undefined;
      runs.push({
        ...base,
        run: next(),
        verdict: null,
        reason: past?.message ?? `${REPLAY_FAILED}: ${messageFor(outcome.reason)}`,
        id: null,
        skillRounds: past?.replayed.rounds ?? null,
        checkoutRounds: past?.replayed.checkoutRounds ?? null,
        skillReads: past?.replayed.reads.map((each) => [...each]) ?? null,
        leftOut: past === undefined ? null : [...past.replayed.leftOut],
        reply: past?.replayed.step.message.content ?? null,
        replaySpent: past?.replayed.usage ?? [],
        deployments: past?.replayed.deployments ?? [],
        judgeSpent: [],
      });
      continue;
    }
    const judgement = judgements.get(`${arm}:${inArm}`);
    if (judgement === undefined || judgement.budgetRefused) {
      budgetSpent = true;
      continue;
    }
    const step: Replayed = outcome.value;
    runs.push({
      ...base,
      run: next(),
      verdict: judgement.verdict,
      reason: judgement.reason,
      id: judgement.id,
      skillRounds: step.rounds,
      checkoutRounds: step.checkoutRounds,
      skillReads: step.reads.map((each) => [...each]),
      ...(step.answeredReplies.length === 0
        ? {}
        : { answeredReplies: step.answeredReplies.map((each) => [...each]) }),
      leftOut: [...step.leftOut],
      ...(step.mixedAct ? { mixedAct: true as const } : {}),
      reply: step.step.message.content,
      replaySpent: step.usage,
      deployments: step.deployments,
      judgeSpent: [...judgement.spent],
    });
  }
  return { runs, budgetSpent };
}

/** Why a written case with no `checkout` answers none of its read-only calls: it was built
 *  before build-cases passed over a context session whose working directory is in no checkout
 *  it knows (build.ts's `no-checkout`). A reply of such calls, its skill reads aside, gets no
 *  verdict (replay.ts's UnansweredRead) and its run is made again. */
const NO_CHECKOUT: Unanswered = {
  why: "the case has no checkout of its context session's repository, so the runner answers none of its read-only calls (build-cases now passes over a context session whose working directory is in no checkout it knows, as no-checkout)",
};

/** What a written case's dotfiles are read through (`caseShell`, `caseKnives`, `caseDotfiles`):
 *  the runner's source and cache, and the configuration's `dotfilesRepo`. */
type DotfilesReader = Pick<Runner, "source" | "cacheDir" | "dotfilesRepo">;

/** The dotfiles' omp wrapper, which exports PI_SHELL_PREFIX from commit bcd580fa on
 *  (checkout.ts's `shellPrefixOf`). */
const DOTFILES_SHIM = "shims/omp";

/** The shell prefix of each dotfiles commit this process has read (`dotfilesPrefix`), by
 *  `<cacheDir>\0<repo>@<commit>`: each read once (its file kept in the cache too), a read that
 *  failed for another reason than a missing file dropped, so a later case reads it again. */
const dotfilesPrefixes = new Map<string, Promise<string | undefined | Unanswered>>();

/** The PI_SHELL_PREFIX the dotfiles' `shims/omp` of `repo` at `commit` exports (checkout.ts's
 *  `shellPrefixOf`; undefined for none), read through the runner's source and cache as a
 *  context's file is; or why not: the file is missing or cannot be read, or sets it in a form the
 *  runner does not read. */
function dotfilesPrefix(
  r: DotfilesReader,
  repo: string,
  commit: string
): Promise<string | undefined | Unanswered> {
  const key = `${r.cacheDir}\0${repo}@${commit}`;
  const held = dotfilesPrefixes.get(key);
  if (held !== undefined) return held;
  const read = (async (): Promise<string | undefined | Unanswered> => {
    let shim: string;
    try {
      shim = await repositoryFile(r.source, r.cacheDir, repo, commit, DOTFILES_SHIM);
    } catch (error) {
      if (!(error instanceof NoSuchFile)) dotfilesPrefixes.delete(key);
      return {
        why: `${DOTFILES_SHIM} of ${repo} at ${commit}, whose PI_SHELL_PREFIX the session's Oh My Pi ran its bash calls with, could not be read: ${messageFor(error)}`,
      };
    }
    const prefix = shellPrefixOf(shim);
    return typeof prefix === "object"
      ? { why: `${DOTFILES_SHIM} of ${repo} at ${commit}: ${prefix.why}` }
      : prefix;
  })();
  dotfilesPrefixes.set(key, read);
  return read;
}

/** The dotfiles' experiments extension (its entry `index.ts`, which installers/omp.sh links into
 *  every profile) and its bash gate's core, whose text tells its version (checkout.ts's
 *  `gateVersion`), and which a commit holds from 050e8a40 on. */
const EXPERIMENTS_DIR = "omp/extensions/experiments";
const GATE_CORE = `${EXPERIMENTS_DIR}/bash-gate-core.ts`;
/** The dotfiles' Oh My Pi settings, which shims/omp gives every session: its `bashInterceptor`
 *  sets Oh My Pi's own interceptor, which a session ran where its commit holds no bash gate
 *  (checkout.ts's `interceptorOf`). */
const DOTFILES_CONFIG = "omp/config.yml";

/** The text of `file` of the dotfiles `repo` at `commit`, through the runner's source and cache;
 *  undefined where the commit holds no such file. Throws where the read fails. */
async function dotfilesText(
  r: DotfilesReader,
  repo: string,
  commit: string,
  file: string
): Promise<string | undefined> {
  try {
    return await repositoryFile(r.source, r.cacheDir, repo, commit, file);
  } catch (error) {
    if (error instanceof NoSuchFile) return undefined;
    throw error;
  }
}

/** Oh My Pi's own bash interceptor as the dotfiles' omp/config.yml of `repo` at `commit` sets it
 *  (checkout.ts's `interceptorOf`: on, with the rules it sets, or undefined where it is off), read
 *  through the runner's source and cache as a context's file is; or why not: the file is missing
 *  or cannot be read, or sets `bashInterceptor` in a form the runner does not read. */
async function dotfilesInterceptor(
  r: DotfilesReader,
  repo: string,
  commit: string
): Promise<BashInterceptor | undefined | Unanswered> {
  const whose = `whose bashInterceptor set the session's Oh My Pi's own bash interceptor`;
  let config: string | undefined;
  try {
    config = await dotfilesText(r, repo, commit, DOTFILES_CONFIG);
  } catch (error) {
    return {
      why: `${DOTFILES_CONFIG} of ${repo} at ${commit}, ${whose}, could not be read: ${messageFor(error)}`,
    };
  }
  if (config === undefined) {
    return { why: `${repo} at ${commit} holds no ${DOTFILES_CONFIG}, ${whose}` };
  }
  const interceptor = interceptorOf(config);
  return interceptor !== undefined && "why" in interceptor
    ? { why: `${DOTFILES_CONFIG} of ${repo} at ${commit}: ${interceptor.why}` }
    : interceptor;
}

/**
 * The shell written case `c`'s context session's Oh My Pi ran its `bash` calls in (checkout.ts's
 * ShellSetup), from the one commit of the configuration's dotfiles repository (`dotfilesRepo`)
 * the case's profile's skills name, and what its stage records of it (StageShell): the
 * PI_SHELL_PREFIX its `shims/omp` exports (`dotfilesPrefix`); and, where that commit holds the
 * experiments extension's bash gate (GATE_CORE), that bash gate (BashGate), at the version the
 * session ran (checkout.ts's `gateVersion`: the profile's commit, or an older one the refusals in
 * the context session's history name, cases.ts's `checkout.bashGate`), its directory read through
 * the runner's source into the cache, in the session's arm; else (every commit before 050e8a40)
 * Oh My Pi's own interceptor as the commit's omp/config.yml sets it (`dotfilesInterceptor`; none
 * where it is off), whatever the case records of a gate, since such a session drew no arm of one.
 * Why not, and then none of the case's `bash` calls is answered, rather than one answered in a
 * shell the session did not have: the configuration names no dotfiles repository, the profile
 * names no commit of it or more than one, the shim or the config cannot be read or sets its value
 * in a form the runner does not read, the gate's core cannot be read, the case of a commit with
 * the gate records no arm of it (a case built before build-cases recorded it is not read from the
 * store, as its `checkout.jj` is not either), `gateVersion` refuses, or the extension cannot be
 * read. Exported for cli.test.ts.
 */
export async function caseShell(
  r: DotfilesReader,
  c: Case
): Promise<{ readonly shell: ShellSetup | Unanswered; readonly record: StageShell }> {
  const refused = (why: string) => ({ shell: { why }, record: { why } });
  const dotfiles = profileDotfiles(
    r,
    c,
    DOTFILES_SHIM,
    "gives the PI_SHELL_PREFIX the session's Oh My Pi ran its bash calls with"
  );
  if ("why" in dotfiles) return refused(dotfiles.why);
  const { repo, commit } = dotfiles;
  const prefix = await dotfilesPrefix(r, repo, commit);
  if (typeof prefix === "object") return refused(prefix.why);
  let profileCore: string | undefined;
  try {
    profileCore = await dotfilesText(r, repo, commit, GATE_CORE);
  } catch (error) {
    return refused(
      `${GATE_CORE} of ${repo} at ${commit}, which says whether the session's bash calls passed through a bash gate, could not be read: ${messageFor(error)}`
    );
  }
  if (profileCore === undefined) {
    const interceptor = await dotfilesInterceptor(r, repo, commit);
    if (interceptor !== undefined && "why" in interceptor) return refused(interceptor.why);
    return {
      shell: {
        ...(prefix === undefined ? {} : { prefix }),
        ...(interceptor === undefined ? {} : { interceptor }),
      },
      record: {
        prefix: prefix ?? null,
        interceptor: { commit, enabled: interceptor !== undefined },
      },
    };
  }
  const recorded = c.checkout?.bashGate;
  if (recorded === undefined) {
    return refused(
      "the case records no arm of its context session's bash gate (checkout.bashGate): it was built before build-cases recorded one, or that session records no experiments entry naming bash_gate_leading before its skill's read; the runner reads no session store for it, as it reads none for checkout.jj"
    );
  }
  try {
    const cores = new Map<string, string>();
    for (const version of recorded.versions === undefined ? [] : BASH_GATE_COMMITS) {
      const text = await dotfilesText(r, repo, version, GATE_CORE);
      if (text !== undefined) cores.set(version, text);
    }
    const version = gateVersion({
      profile: commit,
      profileCore,
      versions: recorded.versions,
      cores,
    });
    if ("why" in version) return refused(version.why);
    const dir = await repositoryDirectory(
      r.source,
      r.cacheDir,
      repo,
      version.commit,
      EXPERIMENTS_DIR
    );
    if (!isFile(path.join(dir, "index.ts"))) {
      return refused(`${repo} at ${version.commit} holds no ${EXPERIMENTS_DIR}/index.ts`);
    }
    return {
      shell: {
        ...(prefix === undefined ? {} : { prefix }),
        gate: { dir, leading: recorded.leading },
      },
      record: { prefix: prefix ?? null, gate: { ...version, leading: recorded.leading } },
    };
  } catch (error) {
    return refused(
      `the experiments extension of ${repo}, whose bash gate the session's bash calls passed through, could not be read: ${messageFor(error)}`
    );
  }
}

/** The one commit of the configuration's dotfiles repository (`dotfilesRepo`) case `c`'s profile's
 *  skills name, whose files set up the session's Oh My Pi (`caseShell`, `caseKnives`) and answer
 *  its reads of its dotfiles checkout (`caseDotfiles`), or why none is known, for the caller's
 *  `file` of it, which `use` says what it is for: the configuration names no dotfiles
 *  repository, or the profile names no commit of it or several. */
function profileDotfiles(
  r: DotfilesReader,
  c: Case,
  file: string,
  use: string
): { repo: string; commit: string } | Unanswered {
  const repo = r.dotfilesRepo;
  if (repo === undefined) {
    return { why: `the configuration names no dotfilesRepo, whose ${file} ${use}` };
  }
  const commits = [
    ...new Set(c.profile.skills.filter((skill) => skill.repo === repo).map((s) => s.commit)),
  ];
  const [commit, ...more] = commits;
  if (commit === undefined || more.length > 0) {
    return {
      why:
        commit === undefined
          ? `the case's profile names no commit of ${repo} (no skill of it), whose ${file} ${use}`
          : `the case's profile names ${commits.length} commits of ${repo} (${commits.join(", ")}), and the runner cannot tell which one's ${file} the session's Oh My Pi ran with`,
    };
  }
  return { repo, commit };
}

/** The dotfiles' knives registry, the `repos.toml` knives reads: its `[repos.<name>]` forks and
 *  its `[trust]` rules (knives v3.2.3 src/config.rs lines 481-540). */
const KNIVES_REGISTRY = "knives/repos.toml";

/** The `owner/name` of the GitHub repository a remote URL names, lower-cased, `.git` taken off, as
 *  knives compares remotes (src/config.rs lines 521-539: case-insensitive, `.git` stripped);
 *  undefined for any other URL. */
function remoteSlug(url: unknown): string | undefined {
  if (typeof url !== "string") return undefined;
  const match =
    /^(?:https?:\/\/|ssh:\/\/git@|git@)github\.com[/:]([^/\s]+)\/([^/\s]+?)(?:\.git)?\/?$/i.exec(
      url.trim()
    );
  return match === null ? undefined : `${match[1]}/${match[2]}`.toLowerCase();
}

/**
 * Why knives' plugin may give blocks to the repository `checkout.repo` checked out at
 * `checkout.root` on written case `c`'s session's machine (the case's checkout, `caseKnives`, or
 * the session's dotfiles checkout, `caseDotfiles`), by the registry the dotfiles hold at the
 * commit the case's profile names (KNIVES_REGISTRY):
 * knives gives a notice where it manages the repository as a fork and guidance where it trusts it
 * (knives v3.2.3 src/hook/resolve.rs lines 108-145; src/commands/hook.rs lines 189-203);
 * undefined where it gives none, as where that commit holds no registry. knives reads the
 * checkout's own remotes, which the runner does not know, so a fork is an entry whose `origin` or
 * `upstream` names a repository of the same name as the case's (a fork whose repository moved, as
 * sjawhar/oh-my-pi did to cybermonklabs/oh-my-pi, among them), and the repository is trusted where
 * the registry's `[trust]` `owners` holds its owner, its `repos` the repository, or its `roots` a
 * directory its root is under, or under `~` where the case records no home (src/config.rs lines
 * 481-540): this may name a repository knives gives none, never the reverse. Also a reason where
 * the registry cannot be read (the profile names no one commit of the dotfiles, or the file is no
 * TOML).
 */
async function knivesMayGive(
  r: DotfilesReader,
  c: Case,
  checkout: Pick<NonNullable<Case["checkout"]>, "repo" | "root">
): Promise<string | undefined> {
  const cannot = (why: string) =>
    `the runner cannot read knives' registry ${KNIVES_REGISTRY}, which says whether knives gives it blocks: ${why}`;
  const dotfiles = profileDotfiles(
    r,
    c,
    KNIVES_REGISTRY,
    "says whether knives gives the checkout's repository blocks"
  );
  if ("why" in dotfiles) return dotfiles.why;
  const at = `${KNIVES_REGISTRY} at ${dotfiles.repo}@${dotfiles.commit}`;
  let registry: Record<string, unknown>;
  try {
    const text = await dotfilesText(r, dotfiles.repo, dotfiles.commit, KNIVES_REGISTRY);
    if (text === undefined) return undefined;
    registry = Object(Bun.TOML.parse(text));
  } catch (error) {
    return cannot(messageFor(error));
  }
  const slug = checkout.repo.toLowerCase();
  const nameOf = (each: string) => each.slice(each.indexOf("/") + 1);
  const fork = Object.entries(Object(registry.repos)).find(([, entry]) =>
    [Object(entry).origin, Object(entry).upstream].some((url) => {
      const remote = remoteSlug(url);
      return remote !== undefined && nameOf(remote) === nameOf(slug);
    })
  )?.[0];
  if (fork !== undefined) return `knives may manage it as its fork ${fork} (${at})`;
  const trust = Object(registry.trust);
  const listed = (key: string): string[] =>
    Array.isArray(trust[key])
      ? trust[key].filter((each: unknown): each is string => typeof each === "string")
      : [];
  const home = sessionHome(checkout.root, c.pathCopy?.home);
  const owner = slug.slice(0, slug.indexOf("/"));
  const trusted =
    listed("owners").some((each) => each.toLowerCase() === owner) ||
    listed("repos").some((each) => each.toLowerCase().replace(/\.git$/, "") === slug) ||
    listed("roots").some((root) => {
      if (root !== "~" && !root.startsWith("~/"))
        return insideRoot(root, checkout.root) !== undefined;
      return (
        home === undefined ||
        insideRoot(path.join(home, root.slice(1)), checkout.root) !== undefined
      );
    });
  return trusted ? `knives trusts it (${at}, its [trust])` : undefined;
}

/**
 * The blocks knives' plugin gave written case `c`'s context session for its checkout's
 * repository (checkout.ts's KnivesRepository, cases.ts's `checkout.knives`), which a run adds to
 * its first answer that touches the repository; undefined where a run adds none: the case has no
 * checkout, the session touched the repository and got no block of it, or knives gives it none
 * (`knivesMayGive`). Why the runner has none where knives may give some: the session never
 * touched the repository, or the case was built before build-cases recorded the blocks (the
 * runner reads no session store for them, as it reads none for `checkout.jj`).
 */
async function caseKnives(r: Runner, c: Case): Promise<KnivesRepository | undefined> {
  const checkout = c.checkout;
  if (checkout === undefined) return undefined;
  const name = checkout.repo;
  const recorded = checkout.knives;
  if (recorded !== undefined && recorded.blocks !== null) return { name, blocks: recorded.blocks };
  if (recorded?.touched === true) return undefined;
  const gives = await knivesMayGive(r, c, checkout);
  if (gives === undefined) return undefined;
  const lacks =
    recorded === undefined
      ? "the case was built before build-cases recorded the blocks its context session got (checkout.knives), and the runner reads no session store for them, as it reads none for checkout.jj"
      : "the case's context session never touched it, so the runner has none of the blocks knives adds to the first result of a session that touches it";
  return { name, blocks: { why: `${gives}, and ${lacks}` } };
}

/**
 * The session's dotfiles checkout as written case `c`'s `read` calls reach it (checkout.ts's
 * DotfilesCheckout), or undefined where the runner knows none, and no read is answered from it:
 * the configuration names no dotfiles repository, the profile names no commit of it or several
 * (`profileDotfiles`), or the copy the profile lists under the case's skill (cases.ts's
 * `testedSource`) is not of it. Its files are read through the runner's source and cache, one
 * directory of the repository at a time (context.ts's `repositoryDirectory`), and whether knives
 * may give the repository blocks by the registry the same commit holds (`knivesMayGive`), once
 * for each root checkout.ts derives. Exported for cli.test.ts.
 */
export function caseDotfiles(r: DotfilesReader, c: Case): DotfilesCheckout | undefined {
  const skill = testedSource(c.profile.skills, c.skill);
  const dotfiles = profileDotfiles(
    r,
    c,
    "files",
    "answer a read of the session's dotfiles checkout"
  );
  if (skill === undefined || "why" in dotfiles || skill.repo !== dotfiles.repo) return undefined;
  const { repo, commit } = dotfiles;
  const knives = new Map<string, Promise<string | undefined>>();
  return {
    repo,
    commit,
    skill: skill.path,
    files: (dir) => repositoryDirectory(r.source, r.cacheDir, repo, commit, dir),
    knives: (root) => {
      const held = knives.get(root) ?? knivesMayGive(r, c, { repo, root });
      knives.set(root, held);
      return held;
    },
  };
}

interface CaseCheckout {
  checkout?: CheckoutCopy | Unanswered;
  record?: StageResult["checkout"];
}

/** Prepare the same copy for fresh runs, continuations, and probes. */
async function caseCheckout(r: Runner, c: Case): Promise<CaseCheckout> {
  if (caseKind(c) !== "written") return {};
  if (c.checkout === undefined) return { checkout: NO_CHECKOUT };
  const { repo, commit, root, cwd } = c.checkout;
  const ranJj = c.checkout.jj ?? messagesRunJj(c.messages);
  const shared = await sharedCheckout(r, repo, commit, ranJj);
  const { git, depth, removedLinks, jj, madeAt } = shared.marker;
  const copyJj = jj === true || ranJj ? jj : undefined;
  const home = sessionHome(root, c.pathCopy?.home);
  const { shell, record } = await caseShell(r, c);
  const knives = await caseKnives(r, c);
  const dotfiles = caseDotfiles(r, c);
  return {
    checkout: {
      dir: shared.dir,
      root,
      cwd,
      git,
      ...(copyJj === undefined ? {} : { jj: copyJj }),
      ...(home === undefined ? {} : { home }),
      madeAt,
      answers: new Map(),
      history: JSON.stringify(c.messages),
      shell,
      ...(knives === undefined ? {} : { knives }),
      ...(dotfiles === undefined ? {} : { dotfiles }),
    },
    record: { git, depth, removedLinks, ...(jj === undefined ? {} : { jj }), shell: record },
  };
}

/** If the copy is missing, replay still handles skill reads, but refuses a checkout round. */
async function continuationCheckout(r: Runner, c: Case): Promise<CaseCheckout> {
  try {
    return await caseCheckout(r, c);
  } catch (error) {
    if (!(error instanceof UnreadableDirectory)) throw error;
    return { checkout: { why: `${UNREADABLE_DIRECTORY}: ${error.message}` } };
  }
}

/**
 * Runs `stage` of case `c` until score.ts's `stageOutcome` says its runs are counted or
 * unscreened, round by round, from the runs `result` holds already (which each caller's
 * `stageDone` has checked were made with this run's texts). Each round's runs are saved when it
 * ends; the stage's last state is returned unsaved, for `finishCase` to write. Each phase's time
 * is counted in `phases`. A case whose context holds a directory no read can give (context.ts's
 * UnreadableDirectory: the commit holds no such directory, or GitHub cuts a level of it short
 * even read alone), or one whose Legion pane's capture defines no `legion` tool in either attempt
 * (capture.ts's LegionToolUndefined, under LEGION_TOOL_UNDEFINED), is returned dropped, with that
 * reason and `stage`, before any run, so a run started again skips it. A stage left unscreened by
 * an arm whose runs read skill files past the last answered round is returned dropped under
 * READS_PAST_LAST_ROUND (`readsPastDrop`); else one
 * in each of whose arms that lack their judged runs the provider refused, on a content policy,
 * every run with no verdict, under PROVIDER_REFUSED (`refusedDrop`). A stage it starts records
 * this runner (`r.runner`), and a confirm it runs or goes on with under `--confirm-flagged`
 * records that (`confirmFlagged`). The runs numbered n of every arm go with one routing id, a
 * random UUID drawn for n the first time this call needs it (so a stage command started again
 * draws its own), and a pinned arm's runs with its `--route-id` pin (`r.routePins`): so round n
 * of runs goes to the deployment its id pins, every arm alike. Throws Stopped, after saving,
 * when the budget refused a call. `place` selects the latest stage by default, or an earlier
 * confirm for which continue-mixed needs fresh runs. A run waiting at FURTHER_ROUND_LIMIT
 * keeps its slot without starting a fresh run.
 */
async function runStage(
  r: Runner,
  c: Case,
  stage: Stage,
  result: CaseResult,
  phases: Phases,
  place: StagePlace = latestPlace(stage)
): Promise<CaseResult> {
  const existing = place.get(result);
  let state: StageResult = existing ?? {
    state: "running",
    sends: SENDS,
    texts: r.texts,
    compared: r.compared,
    plantedLine: stage === "control" ? (r.dirs.plantedLine ?? null) : null,
    judge: judgeIdentity(),
    captures: {},
    seeds: [],
    runs: [],
    usd: 0,
    runner: r.runner,
  };
  if (r.confirmFlagged) state = { ...state, confirmFlagged: true };
  let spentAt = r.gateway.spend().usd;
  let current = result;
  const update = (next: StageResult): void => {
    const now = r.gateway.spend().usd;
    state = { ...next, usd: next.usd + now - spentAt };
    spentAt = now;
    current = place.put(current, state);
  };
  const save = (next: StageResult): string => {
    update(next);
    return writeResult(r, current, phases);
  };

  const scratch = path.join(
    r.scratch,
    "arms",
    createHash("sha256").update(c.id).digest("hex").slice(0, 16)
  );
  const arms = new Map<ArmName, { arm: Arm; capture: Capture }>();
  const captures = { ...state.captures };
  let ours: OursNote | undefined;
  const readsBefore = r.source.reads?.() ?? { calls: 0, ms: 0 };
  // The GitHub reads the captures made, counted into `phases`.
  const countReads = (): void => {
    const readsAfter = r.source.reads?.() ?? { calls: 0, ms: 0 };
    phases.github = {
      calls: readsAfter.calls - readsBefore.calls,
      ms: readsAfter.ms - readsBefore.ms,
    };
  };
  for (const name of stageArms(stage, r.compared)) {
    const started = performance.now();
    const arm = buildArm(c, name, r.dirs, scratch);
    if (arm.ours !== undefined) {
      const { replacedOwn, removedReads } = arm.ours;
      ours = { name: arm.skill, replacedOwn, removedReads: [...removedReads] };
    }
    let capture: Capture;
    try {
      capture = await r.capture(c, arm);
    } catch (error) {
      // An InjectionRefused with no kind is buildArm's, whose faults screen drops before any
      // capture (`injectionDrop`): it fails the command as any other error.
      const dropped =
        error instanceof InjectionRefused && error.kind !== undefined
          ? {
              dropped: `${INJECTION_UNREBUILDABLE}: ${error.kind}: ${error.message}`,
              injectionKinds: [error.kind],
            }
          : error instanceof UnreadableDirectory
            ? { dropped: `${UNREADABLE_DIRECTORY}: ${error.message}` }
            : error instanceof LegionToolUndefined
              ? { dropped: `${LEGION_TOOL_UNDEFINED}: ${error.message}` }
              : undefined;
      if (dropped === undefined) throw error;
      phases.captureMs[name] = performance.now() - started;
      countReads();
      return { ...current, ...dropped, droppedAt: stage };
    }
    phases.captureMs[name] = performance.now() - started;
    if (capture.made) phases.contextMs += capture.record.timings.contextMs;
    // Only the capture reads the arm's messages: the replays and the judge need none.
    arms.set(name, { arm: { ...arm, messages: [] }, capture });
    captures[name] = {
      bodyFile: path.relative(r.out, capture.bodyFile),
      captureDigest: capture.record.captureDigest ?? null,
      sessionDigest: c.sessionDigest,
      // A capture saved before the record kept its blocks holds no `injections`.
      ...((capture.record.injections ?? []).some((each) => each.by === "plugin") &&
      capture.record.dispatch
        ? { dispatch: capture.record.dispatch.from }
        : {}),
    };
  }
  countReads();
  save({ ...state, captures, ...(ours === undefined ? {} : { ours }) });

  let checkout: CheckoutCopy | Unanswered | undefined;
  try {
    const prepared = await caseCheckout(r, c);
    checkout = prepared.checkout;
    if (prepared.record !== undefined) save({ ...state, checkout: prepared.record });
  } catch (error) {
    if (!(error instanceof UnreadableDirectory)) throw error;
    return { ...current, dropped: `${UNREADABLE_DIRECTORY}: ${error.message}`, droppedAt: stage };
  }
  // The routing id of each run number, drawn once in this call (see above).
  const drawn = new Map<number, string>();
  const routingOf = (arm: ArmName, run: number): string => {
    const pinned = r.routePins.get(arm);
    if (pinned !== undefined) return pinned;
    const id = drawn.get(run) ?? randomUUID();
    drawn.set(run, id);
    return id;
  };
  for (;;) {
    const outcome = stageOutcome(state.runs, c.id, stage, r.compared);
    if (outcome.state !== "running") {
      update({ ...state, state: outcome.state });
      const dropped =
        outcome.state === "unscreened"
          ? (readsPastDrop(state.runs, stage, r.compared) ??
            refusedDrop(state.runs, stage, r.compared))
          : undefined;
      return dropped === undefined ? current : { ...current, dropped, droppedAt: stage };
    }
    // A stage whose arms hold their runs but for those that wait (FURTHER_ROUND_LIMIT) starts
    // none: it stays running until the next decision.
    if (Object.values(outcome.toStart).every((count) => count === 0)) {
      update({ ...state, state: "running" });
      return current;
    }
    const seed = newSeed();
    const round = await runRound(
      r,
      c,
      stage,
      arms,
      outcome.toStart,
      state.runs,
      state.seeds.length + 1,
      seed,
      routingOf,
      phases,
      checkout
    );
    const file = save({
      ...state,
      seeds: [...state.seeds, seed],
      runs: [...state.runs, ...round.runs],
    });
    if (round.budgetSpent) {
      throw new Stopped(
        `the budget is spent: the replies so far cost $${r.gateway.spend().usd.toFixed(4)} at list price, so no new model call starts; case ${c.id}'s ${stage} runs so far are saved in ${file}`
      );
    }
  }
}

/** The longest one git command of a shared checkout's making may run: the fetch of a large
 *  commit's history with its blobs, or its checkout (a repository of 115,950 files took 547 s). */
const GIT_TIME_LIMIT_MS = 1_800_000;
/** The depth of a shared checkout's history (`git fetch --depth`): `git log` in it shows at most
 *  this many commits, then the shallow boundary. */
const CHECKOUT_DEPTH = 50;
/** How long a run waits before it looks again at a shared checkout another live run makes. */
const CHECKOUT_WAIT_MS = 2_000;

/** The git URL of the GitHub repository `repo` (`<owner>/<name>`): a written case's fetch's
 *  remote, unless `--repos` names another, and the `origin` of every shared checkout. */
function githubRemote(repo: string): string {
  return `https://github.com/${repo}.git`;
}

/** Runs `git args` with no prompt for a password: its exit code and its error text. */
async function runGit(args: readonly string[]): Promise<{ code: number; stderr: string }> {
  const child = Bun.spawn(["git", ...args], {
    stdin: "ignore",
    stdout: "ignore",
    stderr: "pipe",
    env: { ...process.env, GIT_TERMINAL_PROMPT: "0" },
  });
  const limit = timeLimit(child, GIT_TIME_LIMIT_MS);
  const [stderr, code] = await Promise.all([textOf(child.stderr, limit), child.exited]);
  limit.clear();
  return { code, stderr: limit.timedOut() ? `timed out after ${GIT_TIME_LIMIT_MS} ms` : stderr };
}

/** What a shared checkout's marker records (`sharedCheckout`), written last, once it is made:
 *  whether it is a git checkout (else GitHub's tarball of the commit, with why the git one failed),
 *  the depth of its history, whether it holds the blobs of each commit of it (`blobs`; absent on a
 *  git checkout fetched before with none, which is made again), the symbolic links removed from
 *  it, whether it is a colocated jj repository, the seconds and the bytes its making took, and
 *  when it was made (`madeAt`, milliseconds since the epoch, from which a directory listing of it
 *  measures each entry's age: checkout.ts's `CheckoutCopy.madeAt`; absent on a marker written
 *  before it was recorded, whose copy takes its marker file's time of change, `copyMadeAt`). */
const CheckoutMarker = SharedCheckout.extend({
  blobs: z.literal(true).optional(),
  seconds: z.number().nonnegative(),
  bytes: z.int().nonnegative(),
  why: z.string().optional(),
  madeAt: z.int().nonnegative().optional(),
});
type CheckoutMarker = z.infer<typeof CheckoutMarker>;
/** A marker as `sharedCheckout` gives it: with the time its copy was made (`readCheckoutMarker`). */
type MadeMarker = CheckoutMarker & { readonly madeAt: number };

/** The marker in `markerFile` (CheckoutMarker), or undefined where none is there, with when its
 *  copy was made: the time it records, else, for a marker written before it recorded one, the
 *  whole millisecond of the file's own time of change (it is written last, once the copy is made,
 *  and written again only to colocate the copy, which then records this time), read through the
 *  descriptor its text is read from, so every answer of that copy measures from one time. Read in
 *  one step, so a marker the lock's holder removes as it makes the checkout again is none to a
 *  run that waits for it, never an error between a test of its path and its read. Exported for
 *  cli.test.ts. */
export function readCheckoutMarker(markerFile: string): MadeMarker | undefined {
  let fd: number;
  try {
    fd = openSync(markerFile, "r");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return undefined;
    throw error;
  }
  try {
    const marker = CheckoutMarker.parse(JSON.parse(readFileSync(fd, "utf8")));
    return { ...marker, madeAt: marker.madeAt ?? Math.floor(fstatSync(fd).mtimeMs) };
  } finally {
    closeSync(fd);
  }
}

/** The last ask of this process for each shared checkout, by `<cache dir>\0<repo>@<commit>`: the
 *  next ask waits for it, since a process never waits for its own lock. */
const sharedCheckouts = new Map<string, Promise<unknown>>();

/** The bytes of every file under `dir`, its links counted as links. */
function bytesUnder(dir: string): number {
  let bytes = 0;
  for (const entry of readdirSync(dir, { recursive: true, encoding: "utf8" })) {
    const stats = lstatSync(path.join(dir, entry));
    if (!stats.isDirectory()) bytes += stats.size;
  }
  return bytes;
}

/**
 * The one checkout of `repo` at `commit` that every written case of that commit answers its
 * read-only calls from (replay.ts's checkout rounds), at `<cacheDir>/checkouts/<repo>/<commit>`,
 * read-only once made, and never removed by a stage; and its marker. It is made once, for every
 * run that shares the cache: `git init`, `git remote add origin <r.gitRemote(repo)>`, `git fetch
 * --depth=CHECKOUT_DEPTH origin <commit>`, which brings the blobs of each of those commits (an
 * answer runs with no network, checkout.ts's sandbox, so it could fetch no blob the checkout
 * lacked), `git checkout --detach <commit>`, and `git remote set-url origin <githubRemote(repo)>`,
 * so `git remote -v` and git's errors name the remote a session's checkout names, not `--repos`'
 * directory on the runner (no fetch follows: the answers' git has no transport); where one of
 * them fails, the repository at the commit read into the cache (context.ts's `repositoryTree`:
 * GitHub's tarball through `gh`, or `--repos`' directory) copied there, whose git commands are
 * not answered. Then every symbolic link that resolves outside it, or nowhere, is removed
 * (checkout.ts's `removeOutsideLinks`); where `jj` (the case's session ran jj), the git checkout
 * is made a colocated jj repository (checkout.ts's `colocateJj`), or the marker records why not;
 * and its marker
 * (`<commit>.ready.json`, CheckoutMarker) is written last, so a half-made checkout is never used.
 * A git checkout fetched before with no blobs (its marker records none) is made again. A git
 * checkout made before a case whose session ran jj asked for it is made a colocated jj repository
 * then, once, its marker written again. A lock (`<commit>.lock`, `<pid>`, made whole by
 * a link) keeps a second run from making it at once: a run that finds the lock of a live process
 * waits for the marker; one that finds the lock of a process that runs no more (or its own pid,
 * which no call of its own holds) removes the lock and the half-made checkout and makes it again.
 * Said once, as it is made, with its seconds and size, and once as it is made a jj repository.
 */
function sharedCheckout(
  r: Runner,
  repo: string,
  commit: string,
  jj: boolean
): Promise<{ dir: string; marker: MadeMarker }> {
  const key = `${r.cacheDir}\0${repo}@${commit}`;
  const before = sharedCheckouts.get(key) ?? Promise.resolve();
  const made = before.then(() => makeSharedCheckout(r, repo, commit, jj));
  sharedCheckouts.set(
    key,
    made.catch(() => undefined)
  );
  return made;
}

/** `sharedCheckout`'s making (see there). */
async function makeSharedCheckout(
  r: Runner,
  repo: string,
  commit: string,
  jj: boolean
): Promise<{ dir: string; marker: MadeMarker }> {
  const dir = path.join(r.cacheDir, "checkouts", repo, commit);
  const markerFile = `${dir}.ready.json`;
  const lock = `${dir}.lock`;
  mkdirSync(path.dirname(dir), { recursive: true });
  // A made checkout serves the ask when it holds its commits' blobs (or is the tarball) and,
  // where the ask is for jj, is a colocated jj repository (or is no git checkout).
  const holdsBlobs = (marker: CheckoutMarker) => !marker.git || marker.blobs === true;
  const serves = (marker: CheckoutMarker) =>
    holdsBlobs(marker) && (!jj || !marker.git || marker.jj !== undefined);
  for (;;) {
    const found = readCheckoutMarker(markerFile);
    if (found !== undefined && serves(found)) return { dir, marker: found };
    // The lock, made whole: its pid is written into a file of this process's own, then linked to
    // the lock's name, which fails when the lock is there.
    const mine = `${lock}.${process.pid}.${randomBytes(4).toString("hex")}`;
    const fd = openSync(mine, "wx");
    writeSync(fd, `${process.pid}\n`);
    closeSync(fd);
    let taken = false;
    try {
      linkSync(mine, lock);
      taken = true;
    } catch (error) {
      if (!(error instanceof Error && "code" in error && error.code === "EEXIST")) throw error;
    } finally {
      rmSync(mine, { force: true });
    }
    if (!taken) {
      const pid = Number(readFileSync(lock, "utf8").trim());
      if (pid !== process.pid && Number.isInteger(pid) && pid > 0 && processLives(pid)) {
        await Bun.sleep(CHECKOUT_WAIT_MS);
        continue;
      }
      say(
        r,
        `the checkout ${dir} is half-made by process ${pid}, which holds it no more: it is made again`
      );
      rmSync(lock, { force: true });
      continue;
    }
    try {
      const started = performance.now();
      const seconds = () => Math.round((performance.now() - started) / 100) / 10;
      const made = readCheckoutMarker(markerFile);
      if (made !== undefined && holdsBlobs(made)) {
        if (serves(made)) continue;
        const marker = { ...made, jj: await colocateJj(dir) };
        writeWhole(markerFile, json(marker));
        say(
          r,
          marker.jj === true
            ? `the checkout of ${repo} at ${commit} is made a colocated jj repository in ${seconds()} s: ${dir}`
            : `the checkout of ${repo} at ${commit} could not be made a colocated jj repository (${marker.jj.why}), so no jj command of a session that ran jj is answered: ${dir}`
        );
        return { dir, marker };
      }
      if (made !== undefined) {
        say(
          r,
          `the checkout ${dir} was fetched with no blobs, which no answer may fetch now: it is made again`
        );
      }
      rmSync(markerFile, { force: true });
      rmSync(dir, { recursive: true, force: true });
      mkdirSync(dir, { recursive: true });
      let why: string | undefined;
      for (const step of [
        ["init", "--quiet"],
        ["remote", "add", "origin", r.gitRemote(repo)],
        ["fetch", "--quiet", `--depth=${CHECKOUT_DEPTH}`, "origin", commit],
        ["checkout", "--quiet", "--detach", commit],
        ["remote", "set-url", "origin", githubRemote(repo)],
      ]) {
        const ran = await runGit(["-C", dir, ...step]);
        if (ran.code !== 0) {
          why = `git ${step[0]} failed: ${firstLine(ran.stderr)}`;
          break;
        }
      }
      if (why !== undefined) {
        say(
          r,
          `the checkout of ${repo} at ${commit}: ${why}; GitHub's tarball of the commit answers its reads, and no git command`
        );
        rmSync(dir, { recursive: true, force: true });
        const tree = await repositoryTree(r.source, r.cacheDir, repo, commit);
        cpSync(tree, dir, { recursive: true, verbatimSymlinks: true });
      }
      const removedLinks = removeOutsideLinks(dir);
      const colocated = jj && why === undefined ? await colocateJj(dir) : undefined;
      const marker: MadeMarker = {
        git: why === undefined,
        depth: why === undefined ? CHECKOUT_DEPTH : null,
        ...(why === undefined ? { blobs: true as const } : {}),
        removedLinks,
        ...(colocated === undefined ? {} : { jj: colocated }),
        seconds: seconds(),
        bytes: bytesUnder(dir),
        ...(why === undefined ? {} : { why }),
        madeAt: Date.now(),
      };
      writeWhole(markerFile, json(marker));
      const jjWords =
        colocated === undefined
          ? ""
          : colocated === true
            ? "; a colocated jj repository"
            : `; no jj repository (${colocated.why}), so no jj command of a session that ran jj is answered`;
      say(
        r,
        `the checkout of ${repo} at ${commit} is made in ${marker.seconds} s, ${mib(marker.bytes)} MiB (${marker.git ? `git, its history ${CHECKOUT_DEPTH} commits deep` : "GitHub's tarball, no history"}; ${removedLinks.length === 0 ? "no symbolic link leaves it" : `${removedLinks.length} symbolic links that leave it removed: ${removedLinks.join(", ")}`}${jjWords}): ${dir}`
      );
      return { dir, marker };
    } finally {
      rmSync(lock, { force: true });
    }
  }
}

/** Runs the `--after-case` command `command` on the results file `file` (`<command> "<file>"`
 *  through `sh`, its output the runner's own), ended once `timeoutMs` pass (common.ts's
 *  `timeLimit`). Gives why it failed, naming the file (with the command and the limit when the
 *  limit ended it, `pastLimit`), or undefined when it exited 0. Exported for cli.test.ts. */
export async function runAfterCase(
  command: string,
  file: string,
  timeoutMs = AFTER_CASE_TIMEOUT_MS
): Promise<string | undefined> {
  const child = Bun.spawn(["sh", "-c", `${command} "$1"`, "after-case", file], {
    stdin: "ignore",
    stdout: "inherit",
    stderr: "inherit",
  });
  const limit = timeLimit(child, timeoutMs);
  const code = await child.exited;
  limit.clear();
  if (limit.timedOut()) {
    return `the --after-case command for ${file}: ${pastLimit(command, timeoutMs)}`;
  }
  return code === 0 ? undefined : `the --after-case command exited ${code} for ${file}`;
}

/** Ends case `result`'s stage: records the peak memory, writes the result, says the case's line
 *  (its stage, the peak and the seconds of each phase of `phases`), runs `--after-case`, and stops
 *  the run when the peak is above RUN_MEMORY_LIMIT_BYTES. Returns the result as written. */
async function finishCase(
  r: Runner,
  result: CaseResult,
  stage: Stage,
  phases: Phases
): Promise<CaseResult> {
  const peak = Math.max(r.io.peakRssBytes(), result.peakRssBytes ?? 0);
  const written = { ...result, peakRssBytes: peak };
  const file = writeResult(r, written, phases);
  const what =
    result.dropped === null
      ? stageLine(result.caseId, stage, result.stages[stage])
      : `dropped: ${result.dropped}`;
  say(
    r,
    `case ${result.caseId}: ${what}; peak memory ${mib(peak)} MiB; ${phaseWords(phases)}; written to ${file}`
  );
  if (r.afterCase !== undefined) {
    const failed = await runAfterCase(r.afterCase, file);
    if (failed !== undefined) {
      r.io.err(`${r.command}: ${failed}`);
      r.afterCaseFailures.push(file);
    }
  }
  if (peak > RUN_MEMORY_LIMIT_BYTES) {
    throw new Stopped(
      `the runner's peak resident memory, ${mib(peak)} MiB, is above 1 GiB, so the run stops after case ${result.caseId}, whose results are written to ${file}`
    );
  }
  return written;
}

/** `behaviour screen`. A saved screen of another runner refuses the command before any model call
 *  (`checkRunners`). */
async function screenCommand(r: Runner): Promise<void> {
  const compared =
    r.compared === "none" ? "compared with no skill" : `${r.compared} text ${r.texts[r.compared]}`;
  say(
    r,
    `${r.cases.length} cases of the skill ${r.cases[0]?.skill} in ${r.casesFile}; old text ${r.texts.old}, ${compared}`
  );
  // Each case's result, read once here and kept as each case ends (only this process writes
  // <out>/cases/), for the score that says when screening stops: as the score reads it
  // (`forScore`), so no case's replies are held while the screen runs.
  const results = new Map([...readResults(r)].map(([id, result]) => [id, forScore(result)]));
  checkRunners(r, results.values(), "screen");
  // The score, computed again only once a case has ended: a case passed over changes nothing.
  let scoreSoFar: SkillScore | undefined;
  const finished = async (result: CaseResult, phases: Phases): Promise<void> => {
    results.set(result.caseId, forScore(await finishCase(r, result, "screen", phases)));
    scoreSoFar = undefined;
  };
  // The served-sessions refusal stops the session cases only; the setup refused a skill with no
  // written case before any call.
  const refused = sessionRefusal(r);
  if (refused !== undefined) {
    say(
      r,
      `session cases are not screened: ${refused.reason} (--reading-models ${r.readingModels?.file}); the written cases are`
    );
  }
  const kindOf = kindsOf(r);
  const stopped = new Set<CaseKind>();
  // Session cases first, then written ones: up to MAX_SCREENED_CASES screened cases of each kind,
  // until the skill has MIN_INFORMATIVE_CASES informative cases in all.
  for (const c of screenOrder(r.cases, caseKind)) {
    const kind = caseKind(c);
    if (kind === "session" && refused !== undefined) {
      stopped.add(kind);
      continue;
    }
    const phases = newPhases();
    let result = readResult(r, c.id);
    if (result !== undefined && result.dropped !== null) {
      say(r, `case ${c.id}: dropped before, skipped: ${result.dropped}`);
      continue;
    }
    if (stageDone(r, result, "screen")) {
      // A screen saved unscreened before the provider-refused rule is dropped now, from its runs.
      const refused = result === undefined ? undefined : savedRefusal(result, "screen");
      if (refused === undefined) say(r, `case ${c.id}: screened before, skipped`);
      else await finished(refused, phases);
      continue;
    }
    if (result === undefined) {
      scoreSoFar ??= scored(r, results);
      const score = scoreSoFar;
      const counts = screenCounts(score, kindOf);
      if (counts.informative >= MIN_INFORMATIVE_CASES) {
        say(
          r,
          `screening stops before case ${c.id}: ${score.informative} informative and ${score.screened} screened cases (it stops at ${MIN_INFORMATIVE_CASES} informative in all, or at ${MAX_SCREENED_CASES} screened cases of each kind)`
        );
        return;
      }
      if (screeningDone(counts, kind)) {
        if (!stopped.has(kind)) {
          say(
            r,
            `screening of ${kind} cases stops before case ${c.id}: ${counts.screened[kind]} ${kind} cases screened, ${score.informative} informative in all (it stops at ${MAX_SCREENED_CASES} screened cases of each kind)`
          );
        }
        stopped.add(kind);
        continue;
      }
      result = newResult(c);
      if (servedModel(c.model, r.servedModels) === undefined) {
        const dropped = `${MODEL_NOT_SERVED}: ${notServedWords(c.model, r.servedModels)}`;
        await finished({ ...result, dropped, droppedAt: "screen" }, phases);
        continue;
      }
    }
    const listing = listingStanding(c.evidence);
    if (listing.state === "dropped") {
      await finished({ ...result, dropped: listing.reason, droppedAt: "screen" }, phases);
      continue;
    }
    const injection = injectionDrop(c);
    if (injection !== undefined) {
      const { reason: dropped, kinds: injectionKinds } = injection;
      await finished({ ...result, dropped, droppedAt: "screen", injectionKinds }, phases);
      continue;
    }
    const pathForm = pathFormDrop(c, r.dirs);
    if (pathForm !== undefined) {
      await finished({ ...result, dropped: pathForm, droppedAt: "screen" }, phases);
      continue;
    }
    if (result.controls === null) {
      const controlsStarted = performance.now();
      result = await judgeTheControls(r, c, result);
      phases.controlsMs = performance.now() - controlsStarted;
      if (result.dropped !== null) {
        await finished(result, phases);
        continue;
      }
      writeResult(r, result, phases);
    }
    await finished(await runStage(r, c, "screen", result, phases), phases);
  }
  say(
    r,
    stopped.size === 0
      ? "every case of the file is screened"
      : `the screen is done: it takes no more ${[...stopped].join(" or ")} cases`
  );
}

/** Why `--again <id>` is refused, or undefined: the file holds no such case, or it is dropped, is
 *  not flagged, or has no finished confirm; or it has POOLED_CONFIRMS confirms at this run's
 *  texts on this runner, which the rule pools and after which no confirm runs, or two there that
 *  agree, where the rule runs no third. */
function againRefusal(
  r: Runner,
  score: SkillScore,
  result: CaseResult | undefined,
  id: string
): string | undefined {
  if (!r.cases.some((c) => c.id === id)) {
    return `--again ${id}: ${r.casesFile} holds no case ${id}`;
  }
  const each = score.cases.find((c) => c.caseId === id);
  if (each?.drop !== undefined) {
    return `--again ${id}: case ${id} is dropped at ${each.drop.stage} (${each.drop.reason}), so no confirm of it runs`;
  }
  if (each === undefined || !each.flagged || result === undefined) {
    return `--again ${id}: case ${id} is not flagged by its screen, and only flagged cases are confirmed`;
  }
  const latest = result.stages.confirm;
  if (latest === undefined || latest.state === "running") {
    return `--again ${id}: case ${id} has no finished confirm yet: run behaviour confirm without --again first`;
  }
  const texts = textsKey(r.texts);
  const losses = confirmStages(result).flatMap((stage) => {
    if (stage.runner !== r.runner || textsKey(stage.texts) !== texts) return [];
    const outcome = stageOutcome(stage.runs, id, "confirm", r.compared);
    return outcome.state === "counted" ? [isLoss(outcome.passes, r.compared)] : [];
  });
  if (losses.length >= POOLED_CONFIRMS) {
    return `--again ${id}: case ${id} has ${losses.length} confirms at these texts on this runner, which the rule pools, and no fourth confirm runs`;
  }
  if (losses.length === 2 && losses[0] === losses[1]) {
    return `--again ${id}: case ${id}'s two confirms at these texts on this runner agree (${losses[0] ? "both a loss" : "both no loss"}), and a third confirm runs only where two disagree`;
  }
  return undefined;
}

/** `behaviour confirm`: a confirm of each flagged case that is not dropped and has none; a third
 *  of each whose last two confirms at its texts on one runner disagree (score.ts's
 *  `confirmScore`); and one more of each case `--again` names, where `againRefusal` allows it.
 *  None runs while the skill can get no verdict (`noVerdict`), unless `--confirm-flagged` is given:
 *  then the same confirms run all the same, of flagged cases only, so a case run again alone can be
 *  confirmed, and report gives no skill verdict over them (CONFIRM_FLAGGED_VERDICT). A flagged
 *  case's saved last confirm of another runner refuses the command before any model call
 *  (`checkRunners`); earlier confirms are pooled by `confirmScore`'s rule. */
async function confirmCommand(r: Runner): Promise<void> {
  const { results, score } = screened(r, (s) =>
    s.cases.filter((each) => each.flagged).map((each) => each.caseId)
  );
  const refused = r.again.flatMap((id) => againRefusal(r, score, results.get(id), id) ?? []);
  if (refused.length > 0) throw new Refused(refused.join("; "));
  const why = noVerdict(score);
  if (r.confirmFlagged) {
    say(
      r,
      `--confirm-flagged: each flagged case that is not dropped is confirmed${why === undefined ? "" : `, though the skill gets no verdict (${why})`}; report gives the case results only, and no skill verdict`
    );
  } else if (why !== undefined) {
    say(r, `no confirm runs: the skill gets no verdict: ${why}`);
    return;
  }
  checkRunners(r, results.values(), "confirm");
  const flagged = score.cases.filter((each) => each.flagged);
  const ids = flagged.map((each) => each.caseId);
  say(r, `${ids.length} flagged cases${ids.length === 0 ? "" : `: ${ids.join(", ")}`}`);
  for (const { caseId: id, drop, confirm } of flagged) {
    if (drop !== undefined) {
      say(r, `case ${id}: dropped at ${drop.stage}, so no confirm of it runs: ${drop.reason}`);
      continue;
    }
    const phases = newPhases();
    const c = r.cases.find((each) => each.id === id);
    let result = results.get(id);
    if (c === undefined || result === undefined) throw new Error(`no case ${id} in ${r.casesFile}`);
    if (stageDone(r, result, "confirm")) {
      const refused = savedRefusal(result, "confirm");
      if (refused !== undefined) {
        await finishCase(r, refused, "confirm", phases);
        continue;
      }
      const third = confirm?.state === "running" && confirm.third;
      if (!third && !r.again.includes(id)) {
        say(r, `case ${id}: confirmed before, skipped`);
        continue;
      }
      const { confirm: _latest, ...stages } = result.stages;
      result = { ...result, stages, earlierConfirms: confirmStages(result) };
      say(
        r,
        third
          ? `case ${id}: its last two confirms at these texts on one runner disagree, one a loss and one not, so a third confirm runs, and the three are pooled`
          : `case ${id}: one more confirm runs, as --again asks`
      );
    }
    await finishCase(r, await runStage(r, c, "confirm", result, phases), "confirm", phases);
  }
}

/** `behaviour confirm --case <id>`: FRESH_RUNS fresh runs of old and of the compared arm on case
 *  `c`, with no flag gate and no gate of MIN_INFORMATIVE_CASES informative cases, scored by the
 *  confirm rule; no screen and no judge controls run. The case's saved confirm of another runner
 *  refuses the command before any model call (`checkRunners`). */
async function confirmCaseCommand(r: Runner, c: Case): Promise<void> {
  say(
    r,
    `case ${c.id}: ${FRESH_RUNS} fresh runs of old and of ${r.compared}, with no flag gate and no gate of ${MIN_INFORMATIVE_CASES} informative cases`
  );
  const phases = newPhases();
  const result = readResult(r, c.id) ?? newResult(c);
  checkRunners(r, [result], "confirm");
  if (result.dropped !== null) {
    say(r, `case ${c.id}: dropped before, skipped: ${result.dropped}`);
    return;
  }
  if (stageDone(r, result, "confirm")) {
    say(r, `case ${c.id}: confirmed before, skipped`);
    return;
  }
  await finishCase(r, await runStage(r, c, "confirm", result, phases), "confirm", phases);
}

/** `behaviour control`. The control case's saved control of another runner refuses the command
 *  before any model call (`checkRunners`). */
async function controlStageCommand(r: Runner): Promise<void> {
  const { results, score } = screened(r, (s) => [s.controlCase]);
  const tooFew = skillVerdict({
    informative: score.informative,
    controlLoss: true,
    falseLoss: false,
    losses: 0,
  });
  if (tooFew.verdict === null) {
    say(r, `no control runs: the skill gets no verdict: ${tooFew.reason}`);
    return;
  }
  if (score.noControl !== undefined) {
    say(r, `no control runs: the skill gets no verdict: ${score.noControl}`);
    return;
  }
  // The control case's own work starts here: reading and scoring every case's result is no part
  // of its stage.
  const phases = newPhases();
  const id = score.controlCase;
  const c = r.cases.find((each) => each.id === id);
  const result = id === undefined ? undefined : results.get(id);
  if (c === undefined || result === undefined) throw new Error(`no control case ${id}`);
  checkRunners(r, [result], "control");
  const planted = r.dirs.plantedLine;
  say(
    r,
    `the control case is ${c.id}${replacedWords(score)}; minus leaves out ${planted === undefined ? "the case's line" : `the planted line ${JSON.stringify(planted)}`}, and twin is the old text again (the false-loss control); old, minus and twin are sent in one order each round`
  );
  if (stageDone(r, result, "control")) {
    const refused = savedRefusal(result, "control");
    if (refused === undefined) say(r, `case ${c.id}: control runs made before, skipped`);
    else await finishCase(r, refused, "control", phases);
    return;
  }
  await finishCase(r, await runStage(r, c, "control", result, phases), "control", phases);
}

// `behaviour report`.

/** What a stage's counted passes say, the old text compared with `compared`. */
function stageVerdict(stage: Stage, passes: Passes<Stage>, compared: Compared): string {
  switch (stage) {
    case "screen":
      if (!isInformative(passes)) return "not informative";
      return isFlagged(passes, compared) ? "informative, flagged" : "informative, not flagged";
    case "confirm":
      return isLoss(passes, compared) ? "a loss" : "no loss";
    case "control":
      return `the planted-loss control (old against minus) is ${isControlLoss(passes) ? "a loss" : "no loss"}, the false-loss control (old against twin) is ${isFalseLoss(passes) ? "a loss" : "no loss"}`;
  }
}

/** Each arm of `stage`, the old text compared with `compared` (score.ts's `stageArms`), with how
 *  many of `runs`, a stage's runs, got no verdict for the reads of a reply, the reason beginning
 *  `reason`: READS_PAST_LAST_ROUND, the reply after the last answered round still read skill
 *  files, alone or beside calls that only prepare, or, in a written case, still only made
 *  read-only calls its checkout copy answers (replay.ts's ReadsPastLastRound); UNANSWERED_READ, a
 *  written case's reply made a read-only call its checkout copy answers in no form
 *  (UnansweredRead); or MIXED_UNDECIDED, a reply read skill files beside a call that is neither
 *  an act nor only preparation (UndecidedReply). */
function readsPast(
  runs: readonly Run[],
  stage: Stage,
  compared: Compared,
  reason: string = READS_PAST_LAST_ROUND
): { arm: ArmName; count: number }[] {
  return stageArms(stage, compared).map((arm) => ({
    arm,
    count: runs.filter(
      (run) => run.arm === arm && run.verdict === null && run.reason.startsWith(reason)
    ).length,
  }));
}

/** Counts of arms in words: `old 1, new 0, none 3`. */
function armCounts(counts: readonly { arm: ArmName; count: number }[]): string {
  return counts.map(({ arm, count }) => `${arm} ${count}`).join(", ");
}

/** Why a case whose `stage` is unscreened is dropped, from the stage's `runs` (the old text
 *  compared with `compared`): each arm that lacks its judged runs after its extra runs, with how
 *  many of its runs still only read after the last answered round (skill files, or read-only
 *  calls a written case's checkout answers) and how many made a read-only call the checkout
 *  answers in no form, when it has one or more such runs; then every arm's count of them.
 *  Undefined when no arm that lacks its runs has one: the stage then stays unscreened. */
function readsPastDrop(runs: readonly Run[], stage: Stage, compared: Compared): string | undefined {
  const past = readsPast(runs, stage, compared);
  const unanswered = readsPast(runs, stage, compared, UNANSWERED_READ);
  const short = past.flatMap(({ arm, count }, index) => {
    const missed = unanswered[index]?.count ?? 0;
    const own = runs.filter((run) => run.arm === arm);
    const judged = own.filter((run) => run.verdict !== null).length;
    if (judged >= STAGE_RUNS[stage] || count + missed === 0) return [];
    const which = [
      ...(count > 0
        ? [
            `${count} of which still read after the last answered round (skill files, alone or beside calls that only prepare, or read-only calls a written case's checkout answers)`,
          ]
        : []),
      ...(missed > 0
        ? [`${missed} of which made a read-only call the checkout answers in no form`]
        : []),
    ];
    return [
      `the ${arm} arm has ${judged} of its ${STAGE_RUNS[stage]} judged runs after ${own.length} runs, ${which.join(", ")}`,
    ];
  });
  if (short.length === 0) return undefined;
  const missedWords = unanswered.some(({ count }) => count > 0)
    ? `; runs with a read-only call the checkout answers in no form: ${armCounts(unanswered)}`
    : "";
  return `${READS_PAST_LAST_ROUND}: ${short.join("; ")} (runs that read past the last round: ${armCounts(past)}${missedWords})`;
}

/** How the drop of a case begins whose stage the provider refused on a content policy, in each arm
 *  that lacks its judged runs (`refusedDrop`). */
const PROVIDER_REFUSED = "provider-refused";

/** A run's reason when the provider refused its replay on a content policy: runRound's replay
 *  failure (REPLAY_FAILED) of the stream error gateway.ts throws (`<STREAM_FAILED>: <type>:
 *  <message>`), whose type ends in `_policy`, as the provider's `cyber_policy` does; the type is
 *  group 1. gateway.ts throws it at once; a run saved before it did holds `, on each of <n>
 *  tries` after the message, which this reads alike. Neither prefix holds a character a pattern
 *  reads otherwise. */
const REFUSED_REPLAY = new RegExp(`^${REPLAY_FAILED}: ${STREAM_FAILED}: ([^\\s:]*_policy): `);

/** Why a case whose `stage` is unscreened is dropped because the provider refused it on a content
 *  policy, from the stage's `runs` (the old text compared with `compared`), as `readsPastDrop`
 *  reads an arm: each arm that lacks its judged runs (STAGE_RUNS) is refused when each of its runs
 *  with no verdict is a refusal (REFUSED_REPLAY), and an arm with its judged runs blocks nothing.
 *  The reason names the refused arms joined by ` and ` (no arm where every arm of the stage is
 *  refused), each policy in the order first seen, how many runs were refused, and each arm's
 *  count of them. Undefined when no arm lacks its runs, or one that does has no refusal or a run
 *  with no verdict for another reason: the stage then stays unscreened. */
function refusedDrop(runs: readonly Run[], stage: Stage, compared: Compared): string | undefined {
  const refusals = runs.flatMap((run) => {
    const policy = run.verdict === null ? REFUSED_REPLAY.exec(run.reason)?.[1] : undefined;
    return policy === undefined ? [] : [{ arm: run.arm, policy }];
  });
  const byArm: { arm: ArmName; count: number }[] = [];
  const refused: ArmName[] = [];
  const arms = stageArms(stage, compared);
  for (const arm of arms) {
    const own = runs.filter((run) => run.arm === arm);
    const unjudged = own.filter((run) => run.verdict === null).length;
    const count = refusals.filter((each) => each.arm === arm).length;
    byArm.push({ arm, count });
    if (own.length - unjudged >= STAGE_RUNS[stage]) continue;
    if (count === 0 || count < unjudged) return undefined;
    refused.push(arm);
  }
  if (refused.length === 0) return undefined;
  const of = refused.length === arms.length ? "" : ` of ${refused.join(" and ")}`;
  const policies = [...new Set(refusals.map((each) => each.policy))].join(", ");
  return `${PROVIDER_REFUSED}: provider refused every replay${of} (${policies}): ${refusals.length} refusals (${armCounts(byArm)})`;
}

/** Case `result` dropped at `stage` from its saved runs, where that stage is recorded unscreened
 *  with no drop and the provider refused its arms that lack their runs (`refusedDrop`, by the
 *  compared arm the stage records): a stage saved before that rule, which a stage command that
 *  reaches the case again drops with no model call. Undefined for any other result. */
function savedRefusal(result: CaseResult, stage: Stage): CaseResult | undefined {
  const saved = result.stages[stage];
  if (result.dropped !== null || saved?.state !== "unscreened") return undefined;
  const dropped = refusedDrop(saved.runs, stage, recordedArm(saved));
  return dropped === undefined ? undefined : { ...result, dropped, droppedAt: stage };
}

/** Whether `run` waits for the next decision at continue-mixed's further-round limit: no verdict,
 *  its reason beginning score.ts's FURTHER_ROUND_LIMIT, so no fresh run takes its place. */
function waits(run: Run): boolean {
  return run.verdict === null && run.reason.startsWith(FURTHER_ROUND_LIMIT);
}

/** Case `caseId`'s stage `state` of `stage` as one line, under `label`: each arm's passes and what
 *  they say, by its recorded compared arm. It also counts the reasons for runs with no verdict
 *  by arm. An unfinished stage says what it still starts and what waits at FURTHER_ROUND_LIMIT. */
function stageLine(
  caseId: string,
  stage: Stage,
  state: StageResult | undefined,
  label: string = stage
): string {
  if (state === undefined) return `${label}: not run`;
  const compared = recordedArm(state);
  const outcome = stageOutcome(state.runs, caseId, stage, compared);
  const unjudged = state.runs.filter((run) => run.verdict === null).length;
  const reasons = [READS_PAST_LAST_ROUND, UNANSWERED_READ, MIXED_UNDECIDED];
  const reasonWords = reasons.flatMap((reason) => {
    const counts = readsPast(state.runs, stage, compared, reason);
    return counts.some(({ count }) => count > 0) ? [`; ${reason}: ${armCounts(counts)}`] : [];
  });
  const pastWords = reasonWords.join("");
  const extra = unjudged === 0 ? "" : ` (${unjudged} runs with no verdict, run again${pastWords})`;
  switch (outcome.state) {
    case "counted": {
      const passes = Object.entries(outcome.passes)
        .map(([arm, count]) => `${arm} ${count}/${STAGE_RUNS[stage]}`)
        .join(", ");
      return `${label}: ${passes}: ${stageVerdict(stage, outcome.passes, compared)}${extra}`;
    }
    case "unscreened":
      return `${label}: unscreened: an arm lacks its ${STAGE_RUNS[stage]} judged runs after ${MAX_EXTRA_RUNS} extra runs${extra}`;
    case "running": {
      const toStart = Object.entries(outcome.toStart)
        .map(([arm, count]) => `${arm} ${count}`)
        .join(", ");
      const waiting = stageArms(stage, compared).map((arm) => ({
        arm,
        count: state.runs.filter((run) => run.arm === arm && waits(run)).length,
      }));
      const waitWords = waiting.some(({ count }) => count > 0)
        ? `; waiting at the further-round limit for the next decision: ${armCounts(waiting)}`
        : "";
      return `${label}: not finished, still to start: ${toStart}${waitWords}`;
    }
  }
}

/** A case's pooled confirms (score.ts's `confirmScore`, counted with `pooled`) in words: the
 *  pooled passes of old and of the compared arm `compared`, the gap, and whether it is a loss. */
function pooledWords(pooled: Passes<"confirm">, loss: boolean, compared: Compared): string {
  const runs = FRESH_RUNS * POOLED_CONFIRMS;
  const other = pooled[compared] ?? 0;
  return `old ${pooled.old}/${runs}, ${compared} ${other}/${runs}, ${pooled.old - other} fewer: ${loss ? "a loss" : "no loss"}`;
}

/** Case `result`'s stages as its report line gives them (`stageLine`), each stage it ran; a case
 *  with more than one confirm gives each numbered, those on another runner or at other texts than
 *  its last marked as not counted, then their pooled passes, or the third confirm they call for
 *  (`score`, score.ts's `confirmScore`). */
function caseStageLines(result: CaseResult, score: CaseScore | undefined): string[] {
  const lines: string[] = [];
  const confirms = confirmStages(result);
  const last = confirms.at(-1);
  for (const stage of Stage.options) {
    if (stage !== "confirm" || confirms.length < 2 || last === undefined) {
      const state = stage === "confirm" ? last : result.stages[stage];
      if (state !== undefined) lines.push(stageLine(result.caseId, stage, state));
      continue;
    }
    for (const [n, each] of confirms.entries()) {
      const note =
        each.runner !== last.runner
          ? " (another runner's, not counted)"
          : textsKey(each.texts) !== textsKey(last.texts)
            ? " (at other texts, not counted)"
            : "";
      lines.push(stageLine(result.caseId, stage, each, `confirm ${n + 1}${note}`));
    }
    const confirm = score?.confirm;
    if (confirm?.state === "counted" && confirm.pooled !== undefined) {
      lines.push(`pooled: ${pooledWords(confirm.pooled, confirm.loss, recordedArm(last))}`);
    } else if (confirm?.state === "running" && confirm.third) {
      lines.push("a third confirm is due, since two on one runner disagree (behaviour confirm)");
    }
  }
  return lines;
}

/** The words after a control case that name each dropped case it took the place of, which the
 *  control rule chose before it, or none. */
function replacedWords(score: SkillScore): string {
  if (score.replacedControls.length === 0) return "";
  const named = score.replacedControls.map(
    (id) => `${id} (dropped at ${score.cases.find((each) => each.caseId === id)?.drop?.stage})`
  );
  return `, in place of ${named.join(", ")}, which the control rule chose before it`;
}

/** The served models the screened cases of `kind` of `score` ran on (counted, and not dropped at
 *  screen), each case on the served model of its model's id (`servedModel`), with its screened
 *  cases, most first, a tie by model. */
function screenedModels(
  b: Behaviour,
  score: SkillScore,
  kind: CaseKind
): { model: string; cases: number }[] {
  const cases = new Map<string, number>();
  for (const each of score.cases) {
    const c = b.cases.find((candidate) => candidate.id === each.caseId);
    if (c === undefined || each.screen.state !== "counted" || each.drop?.stage === "screen")
      continue;
    if (caseKind(c) !== kind) continue;
    const model = servedModel(c.model, b.servedModels) ?? c.model;
    cases.set(model, (cases.get(model) ?? 0) + 1);
  }
  return [...cases]
    .map(([model, count]) => ({ model, cases: count }))
    .sort((x, y) => y.cases - x.cases || x.model.localeCompare(y.model));
}

/** The report's line of the screened written cases by the served model each ran on: written
 *  tasks in a recorded session's context, counted apart and never as reading sessions. */
function writtenTasksLine(b: Behaviour, score: SkillScore): string {
  const ran = screenedModels(b, score, "written");
  if (ran.length === 0) return "written tasks: none screened";
  return `written tasks: ${ran.map((item) => `${item.cases} on ${item.model}`).join(", ")} (screened written cases: each a written task in a recorded session's context, never a reading session)`;
}

/** The report's lines on the skill's reading sessions by model (`--reading-models`): each model
 *  of the file with its sessions and whether the gateway serves it, the sessions with no model and
 *  not queried, and the served models the screened session cases ran on; or one line saying they
 *  were not
 *  given. */
function modelLines(b: Behaviour, score: SkillScore): string[] {
  if (b.readingModels === undefined) {
    return [
      "reading sessions by model: not given (no --reading-models), so the verdict names no models",
    ];
  }
  const { file, count } = b.readingModels;
  const ran = screenedModels(b, score, "session")
    .map((item) => `${item.model} ${counted(item.cases, "case")}`)
    .join(", ");
  return [
    `reading sessions by model (--reading-models ${file}): ${counted(count.sessions, "session")}`,
    ...count.byModel.map(
      (item) =>
        `  ${item.model}: ${counted(item.sessions, "session")}, ${item.served ? "served" : "not served"} by the gateway`
    ),
    `  with no model (its file's head names none): ${count.noModel}; not queried (in no models query's rows): ${count.notQueried}`,
    `screened session cases by the served model they ran on: ${ran || "none"}`,
  ];
}

/** The report's line on the skill's counted sessions (`--store-only`): how many of them only the
 *  transcript store holds, with each host's, whatever the verdict; none without `--store-only`. */
function storeOnlyLines(b: Behaviour): string[] {
  const storeOnly = b.storeOnly;
  if (storeOnly === undefined) return [];
  const hosts = storeOnly.hosts.length === 0 ? "" : ` (${storeOnlyHosts(storeOnly)})`;
  return [
    `counted sessions: ${storeOnly.counted}, of which store-only (held by the transcript store alone, so not case sources): ${storeOnly.storeOnly}${hosts}`,
  ];
}

/** The end of a verdict's line with `--reading-models`: the reading sessions the verdict speaks
 *  for, on each served model its screened session cases ran on (a served model of the file counted
 *  under the one of its id), and those it does not: the sessions on models the gateway does not
 *  serve, and any on served models no screened session case ran on. A written case is a written
 *  task, never a reading session, so a skill whose screened cases are all written speaks for no
 *  reading sessions. Empty without `--reading-models`. */
function speaksFor(b: Behaviour, score: SkillScore): string {
  const count = b.readingModels?.count;
  if (count === undefined) return "";
  const spoken = screenedModels(b, score, "session").map(({ model }) => ({
    model,
    id: modelId(model),
    sessions: 0,
  }));
  if (spoken.length === 0) {
    return "; it speaks for no reading sessions: its screened cases are all written tasks, never reading sessions";
  }
  let unserved = 0;
  let otherServed = 0;
  for (const item of count.byModel) {
    const ran = item.served ? spoken.find((each) => each.id === modelId(item.model)) : undefined;
    if (ran !== undefined) ran.sessions += item.sessions;
    else if (item.served) otherServed += item.sessions;
    else unserved += item.sessions;
  }
  const models = spoken
    .map((each) => `${each.model} (${counted(each.sessions, "session")})`)
    .join(", ");
  return (
    `; it speaks for the reading sessions on ${models}; not for the ${counted(unserved, "session")} on models the gateway does not serve` +
    (otherServed === 0
      ? ""
      : `; nor for the ${counted(otherServed, "session")} on served models no screened session case ran on`)
  );
}

/** Where the answer that each case's session had Dispatch comes from, by case id, for each case
 *  one of whose captures carries the Envoy plugin's block of the skill (CaptureNote's `dispatch`,
 *  of any stage). */
function dispatchAnswers(results: ReadonlyMap<string, CaseResult>): Map<string, DispatchFrom> {
  const answers = new Map<string, DispatchFrom>();
  for (const result of results.values()) {
    const stages = [...Object.values(result.stages), ...(result.earlierConfirms ?? [])];
    for (const stage of stages) {
      for (const note of Object.values(stage?.captures ?? {})) {
        if (note.dispatch !== undefined) answers.set(result.caseId, note.dispatch);
      }
    }
  }
  return answers;
}

/** The end of a verdict's line: how many of the skill's screened cases have a listing that could
 *  not be measured (score.ts's `listingStanding`: no listing measure, one it cannot read, or one
 *  that says so), how many installed names their recorded listing gave them (`fromRecorded`), and
 *  how many carry the Envoy plugin's block on the assumption that their session had Dispatch
 *  (`dispatchAnswers`), so a reader sees each rest of the verdict at once. */
function listingWords(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string {
  const screened = score.cases.filter(
    (each) => each.screen.state === "counted" && each.drop?.stage !== "screen"
  );
  const evidence = screened.map(
    (each) => b.cases.find((candidate) => candidate.id === each.caseId)?.evidence
  );
  const unmeasured = evidence.filter(
    (each) => listingStanding(each).state === "not measurable"
  ).length;
  const answers = dispatchAnswers(results);
  const assumed = screened.filter((each) => answers.get(each.caseId) === "assumed").length;
  // A case whose profile took names from its recorded listing (build.ts's POOL_LISTING_RULE):
  // its listing measure cannot see a miss of them.
  const taken = evidence.filter((each) => fromRecordedIn(each).length > 0).length;
  return `; the listing of ${unmeasured} of its ${screened.length} screened cases could not be measured, ${taken} of the ${screened.length} installed names their recorded listing gave them (fromRecorded), and ${assumed} of the ${screened.length} rest on the assumption that their session had Dispatch`;
}

/** The verdict line of a run any of whose confirms `behaviour confirm --confirm-flagged` made:
 *  those confirms ran whatever the skill's informative count and controls, so they give case
 *  results only. */
const CONFIRM_FLAGGED_VERDICT = "case result only; no skill verdict stands (--confirm-flagged)";

/** The skill's verdict as one line: the verdict and its reason, or what it waits for; a verdict
 *  ends with the reading sessions it speaks for (`speaksFor`), and a verdict or its reason with
 *  how many screened cases' listings could not be measured, took names from their recorded
 *  listing, and rest on the assumption that their session had Dispatch (`listingWords`). A run
 *  any of whose confirms, earlier ones too, records `confirmFlagged` gets CONFIRM_FLAGGED_VERDICT
 *  and no verdict. With `--reading-models`, a skill too few of whose reading sessions are on
 *  served models (`servedScreeningRefusal`) gets none when it has no written case; one with
 *  written cases is screened on them, and its reason is theirs. With `--store-only`, the reason
 *  of a skill with fewer than MIN_INFORMATIVE_CASES informative cases ends with how many of its
 *  counted sessions only the transcript store holds, so were never case sources
 *  (`storeOnlyWords`). */
function verdictLine(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string {
  const byOption = [...results.values()].some((result) =>
    confirmStages(result).some((stage) => stage.confirmFlagged === true)
  );
  if (byOption) return CONFIRM_FLAGGED_VERDICT;
  const refusal = skillRefusal(b);
  if (refusal !== undefined) return `none: ${refusal.reason}`;
  const unfinished = screenUnfinished(b, results, score);
  if (unfinished !== undefined) return `not yet: ${unfinished} (behaviour screen)`;
  const outcome = score.outcome;
  if (outcome === undefined) {
    const third = score.cases.flatMap((each) =>
      each.confirm?.state === "running" && each.confirm.third ? [each.caseId] : []
    );
    return score.control?.state === "running"
      ? "not yet: the control runs are not all made (behaviour control)"
      : third.length > 0
        ? `not yet: two confirms on one runner disagree in ${third.join(", ")}, so a third confirm of each is due (behaviour confirm)`
        : "not yet: the confirm runs of a flagged case are not all made (behaviour confirm)";
  }
  const listing = listingWords(b, results, score);
  if (outcome.verdict === null) {
    const controlResult =
      score.controlCase === undefined ? undefined : results.get(score.controlCase);
    const planted = controlResult?.stages.control?.plantedLine ?? null;
    const storeOnly =
      b.storeOnly !== undefined && score.informative < MIN_INFORMATIVE_CASES
        ? storeOnlyWords(b.storeOnly)
        : "";
    return `none: ${reasonWords(outcome.reason, score.control, planted)}${storeOnly}${listing}`;
  }
  const flagged = score.cases.filter((each) => each.flagged).length;
  const control = `the planted-loss control (case ${score.controlCase}) is a loss and its false-loss control is no loss`;
  if (outcome.verdict === "keeps") {
    const kept =
      b.compared === "none"
        ? "no loss of no skill detected by the registered rule"
        : "no loss detected by the registered rule";
    return `${kept}: no confirmed case is a loss (${flagged} flagged of ${score.informative} informative), and ${control}${speaksFor(b, score)}${listing}`;
  }
  // Each case a loss by one confirm, or by POOLED_CONFIRMS pooled ones (score.ts's confirmScore).
  const losses = score.cases.filter((each) => each.loss).map((each) => each.caseId);
  const pooled = score.cases.flatMap((each) =>
    each.loss && each.confirm?.state === "counted" && each.confirm.pooled !== undefined
      ? [each.caseId]
      : []
  );
  const single = losses.filter((id) => !pooled.includes(id));
  const byPool =
    pooled.length === 0
      ? []
      : [
          `in ${pooled.join(", ")}, over ${POOLED_CONFIRMS} pooled confirms, at least ${LOSS_GAP * POOLED_CONFIRMS} fewer of ${FRESH_RUNS * POOLED_CONFIRMS}`,
        ];
  if (b.compared === "none") {
    const where = [
      ...(single.length === 0
        ? []
        : [
            `in ${single.length === losses.length ? "each" : single.join(", ")}, no skill passes at least ${LOSS_GAP} fewer of ${FRESH_RUNS} fresh runs than their skill`,
          ]),
      ...byPool,
    ];
    return `loss: no skill loses against their skill on ${counted(losses.length, "case")} (${losses.join(", ")}): ${where.join("; ")}, and ${control}${speaksFor(b, score)}${listing}`;
  }
  const where = [
    ...(single.length === 0
      ? []
      : [
          `in ${single.join(", ")} ${COMPARED_TEXT[b.compared]} passes at least ${LOSS_GAP} fewer of ${FRESH_RUNS} fresh runs than the old text`,
        ]),
    ...byPool,
  ];
  return `loss: ${where.join("; ")}, and ${control}${speaksFor(b, score)}${listing}`;
}

/** The compared arm as the report's sentences name it. */
const COMPARED_TEXT: Readonly<Record<Compared, string>> = {
  new: "the cut's text",
  ours: "our skill's text",
  none: "no skill",
};

/** The ours arm's notes of `result` (OursNote), one a stage that ran it. */
function oursNotes(result: CaseResult): OursNote[] {
  return Stage.options.flatMap((stage) => result.stages[stage]?.ours ?? []);
}

/** The paths inside the skill of the history reads the ours arm removed from case `result`, each
 *  once, in order. */
function removedReads(result: CaseResult): string[] {
  return [...new Set(oursNotes(result).flatMap((note) => note.removedReads))];
}

/** The report's line on the compared arm: the cut's text; a skill of ours, a bundle where its text
 *  replaced a skill of our name that a case's profile held; or no skill, which informs and clears
 *  nothing. */
function comparedLine(b: Behaviour, results: ReadonlyMap<string, CaseResult>): string {
  if (b.compared === "new") return "compared arm: new (the cut's text)";
  if (b.compared === "none") {
    return "compared arm: none (no skill): the arm with no skill informs and clears nothing";
  }
  const noted = [...results.values()].flatMap((result) =>
    oursNotes(result).map((note) => ({ caseId: result.caseId, note }))
  );
  const name = noted[0]?.note.name;
  const ours = name === undefined ? "ours" : `ours (${name})`;
  const bundled = [...new Set(noted.filter((each) => each.note.replacedOwn).map((e) => e.caseId))];
  if (bundled.length === 0) {
    return `compared arm: ${ours}: their skill hidden, ours in its place under its own name`;
  }
  const cases = new Set(noted.map((each) => each.caseId));
  const some =
    bundled.length === cases.size
      ? ""
      : `, in ${bundled.join(", ")}; in the other cases, ours in their skill's place under its own name`;
  return `compared arm: ${ours}, a bundle: their skill hidden, ours at the given text (it replaced the profile's own ${name})${some}`;
}

/** The report's line of the cases of each kind, and the informative cases of each. */
function kindLine(b: Behaviour, score: SkillScore): string {
  const informative = new Set(score.cases.filter((c) => c.informative).map((c) => c.caseId));
  const words = CaseKind.options.map((kind) => {
    const cases = b.cases.filter((c) => caseKind(c) === kind);
    const of = cases.filter((c) => informative.has(c.id)).length;
    return `${cases.length} ${kind} (${of} informative)`;
  });
  return `cases by kind: ${words.join(", ")}`;
}

/** The screened cases of `score` (counted, and not dropped at screen) with their screen passes, the
 *  old text's and the compared arm's among them (`compared`: no skill's, with no skill compared),
 *  and each case's kind. */
function screenedCases(
  b: Behaviour,
  score: SkillScore
): {
  caseId: string;
  kind: CaseKind;
  passes: Partial<Record<ArmName, number>>;
  informative: boolean;
  old: number;
  compared: number;
}[] {
  const kindOf = kindsOf(b);
  return score.cases.flatMap((each) => {
    if (each.screen.state !== "counted" || each.drop?.stage === "screen") return [];
    const passes: Partial<Record<ArmName, number>> = each.screen.passes;
    return [
      {
        caseId: each.caseId,
        kind: kindOf(each.caseId),
        passes,
        informative: each.informative,
        old: passes.old ?? 0,
        compared: passes[b.compared] ?? 0,
      },
    ];
  });
}

/** The report's lines of each kind's screened cases, a description beside the registered rule:
 *  their ids, how many are informative, and each screen arm's passes summed over them. */
function screenedKindLines(b: Behaviour, score: SkillScore): string[] {
  const screened = screenedCases(b, score);
  return CaseKind.options.map((kind) => {
    const cases = screened.filter((each) => each.kind === kind);
    if (cases.length === 0) return `screened ${kind} cases: none`;
    const passes = stageArms("screen", b.compared)
      .map(
        (arm) =>
          `${arm} ${cases.reduce((sum, each) => sum + (each.passes[arm] ?? 0), 0)}/${cases.length * SCREEN_RUNS}`
      )
      .join(", ");
    const informative = cases.filter((each) => each.informative).length;
    return `screened ${kind} cases: ${cases.length} (${cases.map((each) => each.caseId).join(", ")}), ${informative} informative; screen passes ${passes}`;
  });
}

/** The report's words of the mean of old's screen pass rate less the compared arm's over the
 *  screened cases `cases`, each one observation, with its 95% interval clustered by case
 *  (score.ts's `meanDifference`), or `none` for no case. */
function meanWords(cases: readonly { old: number; compared: number }[]): string {
  const mean = meanDifference(cases.map((each) => (each.old - each.compared) / SCREEN_RUNS));
  if (mean === undefined) return "none";
  const head = `${counted(mean.cases, "case")} ${mean.mean.toFixed(2)}`;
  return mean.interval === undefined
    ? `${head} (one case, so no interval)`
    : `${head} (95% interval ${mean.interval.low.toFixed(2)} to ${mean.interval.high.toFixed(2)}, t over the cases)`;
}

/** The report's line of the mean over the screened cases of old's screen pass rate less the
 *  compared arm's (`meanWords`): over all of them and over each kind's. A description beside the
 *  registered rule, which it does not change. */
function meanLine(b: Behaviour, score: SkillScore): string {
  const screened = screenedCases(b, score);
  return `mean of old less ${b.compared} in screen pass rate, each screened case one observation (a description beside the registered rule): all ${meanWords(screened)}; ${CaseKind.options.map((kind) => `${kind} ${meanWords(screened.filter((each) => each.kind === kind))}`).join("; ")}`;
}

/** The report's line of the screened cases that tie, floor, ceil and reverse, old against the
 *  compared arm in the screen (score.ts's `screenShapes`). */
function shapesLine(b: Behaviour, score: SkillScore): string {
  const shapes = screenShapes(screenedCases(b, score));
  return `screened cases, old against ${b.compared} in the screen: ${counted(shapes.ties, "tie")}, ${counted(shapes.floors, "floor")} (0 of ${SCREEN_RUNS} in both), ${counted(shapes.ceilings, "ceiling")} (${SCREEN_RUNS} of ${SCREEN_RUNS} in both), ${counted(shapes.reverse, "reverse gap")} (${b.compared} above old); a floor or a ceiling is a tie too`;
}

/** The screen runs of each screen arm a screened case must have judged fail on a look
 *  (`judgedOnLook`) to be an inspection floor. */
const FLOOR_RUNS = 8;

/** Whether run `run` was judged on a look: its judged reply's own tool calls (its step less each
 *  answered round's calls, whose answers `skillReads` holds) are one or more, and every one is
 *  read-only (checkout.ts's `readOnlyCall`). */
function judgedOnLook(run: Run): boolean {
  const answered = (run.skillReads ?? []).reduce((sum, round) => sum + round.length, 0);
  const own = toolCalls(run.reply ?? []).slice(answered);
  return own.length > 0 && own.every((call) => readOnlyCall(call.name, call.input));
}

/** The report's lines of the inspection floors, a description beside the registered rule, which
 *  drops no case: the screened cases on which at least FLOOR_RUNS of the SCREEN_RUNS screen runs
 *  of each screen arm are judged fail on a look (`judgedOnLook`), by kind with their ids; and the
 *  mean of old less the compared arm in screen pass rate (`meanWords`) over all screened cases and
 *  over those that are not floors. */
function inspectionLines(
  b: Behaviour,
  score: SkillScore,
  results: ReadonlyMap<string, CaseResult>
): string[] {
  const screened = screenedCases(b, score);
  const arms = stageArms("screen", b.compared);
  const floors = screened.filter(({ caseId }) => {
    const runs = results.get(caseId)?.stages.screen?.runs ?? [];
    return arms.every(
      (arm) =>
        runs.filter((run) => run.arm === arm && run.verdict === "fail" && judgedOnLook(run))
          .length >= FLOOR_RUNS
    );
  });
  const byKind = CaseKind.options
    .map((kind) => {
      const ids = floors.filter((each) => each.kind === kind).map((each) => each.caseId);
      return `${kind} ${ids.length}${ids.length === 0 ? "" : `: ${ids.join(", ")}`}`;
    })
    .join("; ");
  const others = screened.filter((each) => !floors.includes(each));
  const named = others.length === 0 ? "" : ` (${others.map((each) => each.caseId).join(", ")})`;
  return [
    `inspection floors (screened cases on which at least ${FLOOR_RUNS} of the ${SCREEN_RUNS} screen runs of each screen arm are judged fail on a step whose own tool calls are all read-only calls; a description beside the registered rule, which drops no case): ${floors.length} of ${screened.length} screened (${byKind})`,
    `mean of old less ${b.compared} in screen pass rate, each screened case one observation (a description beside the registered rule): over all ${counted(screened.length, "screened case")} ${meanWords(screened)}; over the ${counted(others.length, "screened case")} that are not inspection floors${named} ${meanWords(others)}`,
  ];
}

/** The files of `files` (paths inside `old` of its files other than SKILL.md) that case `c`'s
 *  real step reads, each once, in order, as the arms tell a read of a file of the skill (cases.ts's
 *  `stepSkillFiles`: a `skill://` read, or a whole read by path of the case's own copy, its
 *  relative or `~` path resolved), at the installed directory the arms show (`buildArm`'s, of the
 *  arm with no skill, which reads no directory and writes nothing). */
function stepSkillReads(c: Case, old: string, files: readonly string[]): string[] {
  const arm = buildArm(c, "none", { old }, path.join(tmpdir(), SCRATCH_DIR));
  return stepSkillFiles(c.realStep, arm, arm.installedDir, files);
}

/** The report's line of the screened cases whose real step reads a file of the skill under `old`
 *  other than SKILL.md (`stepSkillReads`), each with the files it reads: the arm with no skill
 *  does not have that file, so it fails such a step whatever the skill's text teaches; each
 *  screened case whose arms `buildArm` refuses, as one whose reads cannot be told, with the first
 *  line of why, counted in neither; and how many of the other screened cases are informative,
 *  flagged and confirmed losses. A description beside the registered rule, which it does not
 *  change. */
function stepReadsLine(b: Behaviour, score: SkillScore, old: string): string {
  const files = skillFilePaths(old).filter((file) => file !== "SKILL.md");
  const screened = score.cases.filter(
    (each) => each.screen.state === "counted" && each.drop?.stage !== "screen"
  );
  const reading: string[] = [];
  const untold: string[] = [];
  const others: CaseScore[] = [];
  for (const each of screened) {
    const c = b.cases.find((candidate) => candidate.id === each.caseId);
    let read: string[];
    try {
      read = c === undefined ? [] : stepSkillReads(c, old, files);
    } catch (error) {
      const why = messageFor(error).split("\n")[0];
      untold.push(`; ${each.caseId}: its reads cannot be told (${why})`);
      continue;
    }
    if (read.length === 0) others.push(each);
    else reading.push(`${each.caseId} reads ${read.join(" and ")}`);
  }
  const which =
    reading.length === 0
      ? `none of ${screened.length} screened`
      : `${reading.length} of ${screened.length} screened (${reading.join("; ")})`;
  const count = (keep: (each: CaseScore) => boolean) => others.filter(keep).length;
  return `cases whose real step reads a file of the skill other than SKILL.md, which the arm with no skill does not have, so it fails them by construction: ${which}${untold.join("")}; the ${counted(others.length, "screened case")} with no such read: ${count((each) => each.informative)} informative, ${count((each) => each.flagged)} flagged, ${count((each) => each.loss)} confirmed losses (a description beside the registered rule, which it does not change)`;
}

/** What the report reads of the build-cases summaries (`--build-summary`): their navigation lines,
 *  and the id of each replacement case their session records name. */
interface BuildNavigation {
  readonly lines: NavigationCounts["lines"];
  readonly replacements: readonly string[];
}

/** The navigation lines the build-cases summaries `files` name (`--build-summary`, build.ts's
 *  `NavigationSummary`), each line of a file once, and the replacement cases their session records
 *  name (each `navigation` entry's `replacement` that is not null), each once, in the order the
 *  files name them. Refuses a file that is no build-cases summary, or one written before
 *  build-cases named them. */
function buildNavigation(files: readonly string[]): BuildNavigation {
  const lines = new Map<string, NavigationCounts["lines"][number]>();
  const replacements = new Set<string>();
  for (const file of files) {
    const parsed = NavigationSummary.safeParse(readJson(file, "--build-summary"));
    if (!parsed.success) {
      throw new Refused(
        `--build-summary ${file} is no build-cases summary that names its navigation lines: ${schemaFaults(parsed.error, "the file")}`
      );
    }
    for (const line of parsed.data.navigation.lines) {
      const key = `${line.file}\0${line.number}`;
      if (!lines.has(key)) lines.set(key, line);
    }
    for (const session of parsed.data.sessions ?? []) {
      for (const { replacement } of session.navigation ?? []) {
        if (replacement !== null) replacements.add(replacement);
      }
    }
  }
  return { lines: [...lines.values()], replacements: [...replacements] };
}

/** The report's line of the navigation lines of the build summaries (`buildNavigation`) whose
 *  file and text, trimmed, no case of `b` has as its line: lines whose steps a build dropped, which
 *  no case tests. A description beside the registered rule, which it does not change. */
function untestedNavigationLine(b: Behaviour, navigation: NavigationCounts["lines"]): string {
  const untested = navigation.filter(
    (line) =>
      !b.cases.some((c) => c.baseFile === line.file && c.line.text.trim() === line.text.trim())
  );
  const words = untested.map((line) => `${line.file}:${line.number} ${JSON.stringify(line.text)}`);
  return `navigation lines of the skill that no case tests (from the build summaries): ${words.join("; ") || "none"}`;
}

/** The report's line of the replacement cases of the build summaries (`buildNavigation`): how
 *  many, and how many of them the judge controls leave, which is all but those whose result in
 *  `results` a judge control dropped at the screen (a case with no result yet counts, as the build
 *  counted it); it names each case the controls dropped and each with no result. A description
 *  beside the registered rule, which it does not change. */
function replacementLine(
  results: ReadonlyMap<string, CaseResult>,
  replacements: readonly string[]
): string {
  let kept = 0;
  const named: string[] = [];
  for (const id of replacements) {
    const result = results.get(id);
    if (result !== undefined && result.dropped !== null && result.controls?.hold === false) {
      named.push(`${id} dropped by the judge controls`);
      continue;
    }
    kept += 1;
    if (result === undefined) named.push(`${id} not screened yet`);
  }
  const which = named.length === 0 ? "" : ` (${named.join("; ")})`;
  return `navigation drops with a replacement case: ${replacements.length} by the build summaries, ${kept} after the judge controls${which}`;
}

/** The report's line of the judge's identity (judge.ts's `judgeIdentity`): the SHA-256 of
 *  `prompts/judge.md` and its pinned settings, and how many stages of the results record it,
 *  record another, and record none (a stage made before the identity was recorded). */
function judgeLine(results: ReadonlyMap<string, CaseResult>): string {
  const now = judgeIdentity();
  const settings = (identity: JudgeIdentity) =>
    Object.entries(identity.settings)
      .map(([key, value]) => `${key} ${value}`)
      .join(", ");
  let stages = 0;
  let same = 0;
  let none = 0;
  const others = new Map<string, number>();
  for (const result of results.values()) {
    for (const stage of Stage.options) {
      const state = result.stages[stage];
      if (state === undefined) continue;
      stages += 1;
      const judge = state.judge;
      if (judge === undefined) none += 1;
      else if (judge.sha256 === now.sha256 && settings(judge) === settings(now)) same += 1;
      else {
        const other = `sha256 ${judge.sha256} (${settings(judge)})`;
        others.set(other, (others.get(other) ?? 0) + 1);
      }
    }
  }
  const other = stages - same - none;
  const which =
    others.size === 0
      ? ""
      : ` (${[...others].map(([each, count]) => `${each} by ${count}`).join("; ")})`;
  return `judge: prompts/judge.md sha256 ${now.sha256}; ${settings(now)}; recorded by ${same} of the ${stages} stages of the results, another judge by ${other}${which}, none by ${none}${none === 0 ? "" : " (judged before the judge's identity was recorded)"}`;
}

/** The report's line of each model of the cases, with its cases, most first, a tie by model. */
function caseModelsLine(b: Behaviour): string {
  const models = new Map<string, number>();
  for (const c of b.cases) models.set(c.model, (models.get(c.model) ?? 0) + 1);
  const words = [...models]
    .sort(([x, a], [y, z]) => z - a || x.localeCompare(y))
    .map(([model, cases]) => `${model} ${counted(cases, "case")}`);
  return `models of the cases: ${words.join(", ") || "none"}`;
}

/** The report's two lines of ours against old (their skill): each arm's screen passes, and the
 *  confirm passes and losses where confirm runs are counted, summed over the screened cases, then
 *  over those from whose history the ours arm removed no read. */
function oursAgainstOldLines(
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string[] {
  const screened = score.cases.filter(
    (each) => each.screen.state === "counted" && each.drop?.stage !== "screen"
  );
  const clean = screened.filter((each) => {
    const result = results.get(each.caseId);
    return result === undefined || removedReads(result).length === 0;
  });
  const sums = (cases: readonly CaseScore[]): string => {
    /** The passes of `arm` summed over the counted stages of `stages`. */
    const passes = (arm: ArmName, stages: readonly (Partial<Record<ArmName, number>> | null)[]) =>
      stages.reduce((sum, each) => sum + (each?.[arm] ?? 0), 0);
    const screens = cases.flatMap(({ screen }) =>
      screen.state === "counted" ? [screen.passes] : []
    );
    const screen = stageArms("screen", "ours")
      .map((arm) => `${arm} ${passes(arm, screens)}/${cases.length * SCREEN_RUNS}`)
      .join(", ");
    // Each counted confirm's passes and runs: its POOLED_CONFIRMS pooled ones', or its last one's.
    const confirms = cases.flatMap(({ confirm }) =>
      confirm?.state !== "counted"
        ? []
        : confirm.pooled === undefined
          ? [{ passes: confirm.counted.at(-1) ?? null, runs: FRESH_RUNS }]
          : [{ passes: confirm.pooled, runs: FRESH_RUNS * POOLED_CONFIRMS }]
    );
    const runs = confirms.reduce((sum, each) => sum + each.runs, 0);
    const losses = cases.filter((each) => each.loss).length;
    const confirm =
      confirms.length === 0
        ? "no confirm runs"
        : `confirm on ${counted(confirms.length, "case")}: ${stageArms("confirm", "ours")
            .map(
              (arm) =>
                `${arm} ${passes(
                  arm,
                  confirms.map((each) => each.passes)
                )}/${runs}`
            )
            .join(", ")}, ${losses} a loss, ${confirms.length - losses} no loss`;
    return `screen ${screen}; ${confirm}`;
  };
  const ids = clean.length === 0 ? "" : ` (${clean.map((each) => each.caseId).join(", ")})`;
  return [
    `ours against old (their skill), over all ${counted(screened.length, "screened case")}: ${sums(screened)}`,
    `ours against old (their skill), over the ${counted(clean.length, "screened case")} with no removed read${ids}: ${sums(clean)}`,
  ];
}

/** A smallest drop's figure, as the report prints it. */
function dropWords(drop: SmallestDrop): string {
  const percent = `${(100 * drop.chance).toFixed(2)}%`;
  if (drop.reaches) return `${drop.points} points (${percent})`;
  if (drop.points === 0) return "no drop, since old passes none of its screen runs";
  return `no drop down to 0 reaches ${100 * DROP_CHANCE}% (${percent} at ${drop.points} points)`;
}

/** The report's lines of the smallest drop (score.ts's `dropFigures`): each screened case's, and
 *  the median over the screened cases and over the informative ones. */
function dropLines(b: Behaviour, score: SkillScore): string[] {
  const figures = dropFigures(score, b.compared);
  const median = (points: number | undefined) =>
    points === undefined
      ? "none"
      : points === Number.POSITIVE_INFINITY
        ? `no drop down to 0 reaches ${100 * DROP_CHANCE}%`
        : `${points} points`;
  const informative = figures.cases.filter((each) => each.informative).length;
  return [
    `smallest drop the rule catches with a chance of at least ${100 * DROP_CHANCE}%, by screened case (${DROP_STEP_POINTS}-point steps below the case's screened pass rate of old; the rule's chance with the compared arm ${b.compared}):`,
    ...figures.cases.map(
      ({ caseId, drop }) => `  ${caseId} old ${(drop.old / 100).toFixed(1)}: ${dropWords(drop)}`
    ),
    `median smallest drop: ${median(figures.screened)} over the ${counted(figures.cases.length, "screened case")}; ${median(figures.informative)} over the ${counted(informative, "informative case")}`,
  ];
}

/** Each stage `result` ran, its earlier confirms among them, with its kind. */
function eachStage(result: CaseResult): [Stage, StageResult][] {
  const stages: [Stage, StageResult][] = [];
  for (const stage of Stage.options) {
    const state = result.stages[stage];
    if (state !== undefined) stages.push([stage, state]);
  }
  for (const state of result.earlierConfirms ?? []) stages.push(["confirm", state]);
  return stages;
}

/** A runner's share of one stage kind: the case stages it made runs of, and those runs. */
interface RunnerShare {
  readonly stages: number;
  readonly runs: number;
}

/** Each runner of each stage kind of `results`, earlier confirms among them, with its share
 *  (`stageRunners`): a stage whose runs two runners made counts as a case stage of each. */
function runnerCounts(results: Iterable<CaseResult>): Record<Stage, Map<string, RunnerShare>> {
  const counts: Record<Stage, Map<string, RunnerShare>> = {
    screen: new Map(),
    confirm: new Map(),
    control: new Map(),
  };
  for (const result of results) {
    for (const [stage, state] of eachStage(result)) {
      for (const [runner, runs] of stageRunners(state)) {
        const share = counts[stage].get(runner) ?? { stages: 0, runs: 0 };
        counts[stage].set(runner, { stages: share.stages + 1, runs: share.runs + runs });
      }
    }
  }
  return counts;
}

/** The report's line of the runners of `results`: for each stage kind, each runner with its case
 *  stages and runs (`runnerCounts`). Refuses results whose runs more than one runner made unless
 *  `--allow-runners` names each, naming each runner with its counts: a report of them pools runs
 *  of different code, so it says so only when told to, and then says they were allowed. */
function runnerLine(b: Behaviour, results: ReadonlyMap<string, CaseResult>): string {
  const counts = runnerCounts(results.values());
  const words = Stage.options
    .map((stage) => {
      const each = [...counts[stage]].map(
        ([runner, share]) =>
          `${runner} (${counted(share.stages, "case stage")}, ${counted(share.runs, "run")})`
      );
      return `${stage}: ${each.join(", ") || "none"}`;
    })
    .join("; ");
  const runners = [...new Set(Stage.options.flatMap((stage) => [...counts[stage].keys()]))];
  if (runners.length < 2) return `runners: ${words}`;
  const unnamed = runners.filter((runner) => !b.allowRunners.includes(runner));
  if (unnamed.length > 0) {
    throw new Refused(
      `the case results in ${path.join(b.out, "cases")} were made by ${runners.length} runners, so report counts none of them: ${words}; ${b.allowRunners.length === 0 ? "no --allow-runners is given" : `--allow-runners does not name ${unnamed.join(", ")}`}. Give --allow-runners ${runners.join(",")} to report them together`
    );
  }
  return `runners: ${words}; ${runners.length} runners, each allowed by --allow-runners, so their results are reported together`;
}

/** Whether a sum of dollars is none to the four places the report prints. */
const noDollars = (amount: number) => Math.abs(amount) < 0.00005;

/** Dollars as the report prints them: `$1.2345`, never `$-0.0000`. */
const dollars = (amount: number) => `$${(noDollars(amount) ? 0 : amount).toFixed(4)}`;

/** What one stage kind's calls cost as the case results record it: its case stages and their
 *  runs, the runs with no verdict among them (a run with no verdict is run again, and its calls
 *  were paid), the dollars its runs record for their replays and for their judgements, and its
 *  stages' dollars (StageResult's `usd`, the gateway's spend while each ran), whose rest no run
 *  records. */
interface StageSpend {
  stages: number;
  runs: number;
  unjudged: number;
  replays: number;
  judgements: number;
  usd: number;
}

/** What the case results record their calls cost: the judge controls and the cases that ran them,
 *  each stage kind's (StageSpend), and the sum, which is the report's spend. */
interface RecordedSpend {
  controlled: number;
  controls: number;
  readonly stages: Record<Stage, StageSpend>;
  total: number;
}

/** The spend `results` record (RecordedSpend): each result's judge controls (`controls.usd`, the
 *  gateway's spend over them) and each stage it ran, earlier confirms among them, each call once,
 *  since each process counts a call into the one stage or controls it was made for. */
function recordedSpend(results: Iterable<CaseResult>): RecordedSpend {
  const kind = (): StageSpend => ({
    stages: 0,
    runs: 0,
    unjudged: 0,
    replays: 0,
    judgements: 0,
    usd: 0,
  });
  const spend: RecordedSpend = {
    controlled: 0,
    controls: 0,
    stages: { screen: kind(), confirm: kind(), control: kind() },
    total: 0,
  };
  for (const result of results) {
    if (result.controls !== null) {
      spend.controlled += 1;
      spend.controls += result.controls.usd;
    }
    for (const [stage, state] of eachStage(result)) {
      const each = spend.stages[stage];
      each.stages += 1;
      each.runs += state.runs.length;
      for (const run of state.runs) {
        if (run.verdict === null) each.unjudged += 1;
        each.replays += usdOf(run.replaySpent);
        each.judgements += usdOf(run.judgeSpent);
      }
      each.usd += state.usd;
    }
  }
  spend.total = Stage.options.reduce((sum, stage) => sum + spend.stages[stage].usd, spend.controls);
  return spend;
}

/** The report's spend at list price: the sum of what the case results under `b.out` record their
 *  calls cost (`recordedSpend`), and each part of it in words: the judge controls; and for each
 *  stage kind its runs' replays and judgements, and the calls no run records (its stages' dollars
 *  less its runs': its captures' hint calls, the rounds a replay that then failed had answered,
 *  replies whose judgement the budget refused). */
function spendLines(b: Behaviour, spend: RecordedSpend): string[] {
  const { stages } = spend;
  return [
    `spend at list price: ${dollars(spend.total)}, the sum of what the case results in ${path.join(b.out, "cases")} record their calls cost (judge controls ${dollars(spend.controls)}, screen ${dollars(stages.screen.usd)}, confirm ${dollars(stages.confirm.usd)}, control ${dollars(stages.control.usd)}):`,
    `  judge controls: ${dollars(spend.controls)}, the judge controls of ${counted(spend.controlled, "case")}`,
    ...Stage.options.map((stage) => {
      const each = stages[stage];
      return `  ${stage}: ${dollars(each.usd)} over ${counted(each.stages, "case stage")}: ${dollars(each.replays)} for the replays and ${dollars(each.judgements)} for the judgements of their ${counted(each.runs, "run")} (${each.unjudged} with no verdict), and ${dollars(each.usd - each.replays - each.judgements)} for the calls no run records (its captures' hint calls, the rounds a replay that then failed had answered, replies whose judgement the budget refused)`;
    }),
  ];
}

/**
 * The report's lines on the request log `file` (`loggedGateway`'s lines of `behaviour`: kinds
 * `judge`, `replay`, `hint`, and `behaviour continue-mixed`'s `continuation` and `probe`), set
 * against the spend the case results record (`spend`): the log's dollars and calls by kind, and
 * its difference from the results' sum in two parts that sum to it, each with its cause: the judge
 * calls against the results' judge controls and judgements, and the other calls against the
 * results' replays and the calls no run records. A replay or continuation call goes with its
 * run's session id, which the run records (`userId`), so the replay calls of runs no result holds,
 * and the runs of the results no replay call of the log holds, are counted exactly; a judge or
 * hint call names no run. A line that is no JSON (the end of a line a process was stopped in the
 * middle of) is counted and left out. Where the file does not exist, one line says so.
 */
function requestLogLines(
  file: string,
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  spend: RecordedSpend
): string[] {
  if (!existsSync(file)) return [`request log: none at ${file}, so the spend is set against none`];
  const kinds = new Map<string, { calls: number; usd: number }>();
  const replays: { userId: string | null; usd: number }[] = [];
  let unreadable = 0;
  // A line that is no JSON (`parseJson`'s undefined) is counted and left out.
  for (const { value: line } of jsonLines(file)) {
    if (line === undefined) {
      unreadable += 1;
      continue;
    }
    const kind = String(field(line, "kind"));
    const usd = field(line, "usd");
    const amount = typeof usd === "number" ? usd : 0;
    const each = kinds.get(kind) ?? { calls: 0, usd: 0 };
    kinds.set(kind, { calls: each.calls + 1, usd: each.usd + amount });
    if (kind === "replay" || kind === "continuation") {
      const userId = field(line, "userId");
      replays.push({ userId: typeof userId === "string" ? userId : null, usd: amount });
    }
  }
  const runs = [...results.values()].flatMap((result) =>
    eachStage(result).flatMap(([, state]) =>
      state.runs.map((run) => ({
        userId: run.userId,
        usd: usdOf(run.replaySpent) + usdOf(run.judgeSpent),
      }))
    )
  );
  const runIds = new Set(runs.flatMap((run) => (run.userId === null ? [] : [run.userId])));
  const logIds = new Set(replays.flatMap((each) => (each.userId === null ? [] : [each.userId])));
  const strays = replays.filter((each) => each.userId === null || !runIds.has(each.userId));
  const unlogged = runs.filter((run) => run.userId === null || !logIds.has(run.userId));
  const sum = (items: readonly { usd: number }[]) => items.reduce((all, each) => all + each.usd, 0);

  const total = sum([...kinds.values()]);
  const calls = [...kinds.values()].reduce((all, each) => all + each.calls, 0);
  const loggedJudge = kinds.get("judge")?.usd ?? 0;
  const recordedJudge = Stage.options.reduce(
    (all, stage) => all + spend.stages[stage].judgements,
    spend.controls
  );
  const cases = path.join(b.out, "cases");
  const elsewhere = "results made with another --request-log, or brought from another --out";
  const less = `the results record calls this log does not hold: ${elsewhere}`;
  /** One part of the difference: the log's dollars of `what` against the results'. */
  const part = (what: string, logged: number, recorded: number, more: string): string => {
    const difference = logged - recorded;
    const words = noDollars(difference)
      ? "the same in both"
      : `${dollars(Math.abs(difference))} ${difference > 0 ? `more in the log: ${more}` : `less in the log: ${less}`}`;
    return `  ${what}: ${dollars(logged)} in the log, ${dollars(recorded)} in the results, ${words}`;
  };
  const difference = total - spend.total;
  const byKind = [...kinds]
    .map(([kind, each]) => `${kind} ${counted(each.calls, "call")} ${dollars(each.usd)}`)
    .join(", ");
  const strayWords =
    strays.length === 0
      ? ""
      : `; ${counted(strays.length, "replay call")} of the log, ${dollars(sum(strays))}, went with a session id no run of the results holds: runs of a round stopped before it was saved, of a result no longer in ${cases}, or replies whose judgement the budget refused`;
  const unloggedWords =
    unlogged.length === 0
      ? ""
      : `; ${counted(unlogged.length, "run")} of the results, ${dollars(sum(unlogged))}, went with a session id no replay call of the log holds: ${elsewhere}`;
  return [
    `request log ${file}: ${dollars(total)} over ${counted(calls, "call")} (${byKind || "none"})${unreadable === 0 ? "" : `, ${counted(unreadable, "line")} that is no JSON left out`}, ${noDollars(difference) ? "the same as" : `${dollars(Math.abs(difference))} ${difference > 0 ? "more" : "less"} than`} the spend the case results record; by kind of call:`,
    part(
      "judge (the results' judge controls and judgements)",
      loggedJudge,
      recordedJudge,
      `judge calls no saved result records: the judge controls or judgements of a case stopped before they were saved, or of a result no longer in ${cases}`
    ),
    part(
      "replay and hint (the results' replays and the calls no run records)",
      total - loggedJudge,
      spend.total - recordedJudge,
      `calls no saved stage records: a round stopped before it was saved, a result no longer in ${cases}, or the hint calls of a capture that failed`
    ) +
      strayWords +
      unloggedWords,
  ];
}

/** Every run of stage kind `stage` that `results` hold, each confirm's (the earlier ones too). */
function kindRuns(results: ReadonlyMap<string, CaseResult>, stage: Stage): Run[] {
  return [...results.values()].flatMap((result) => {
    const stages: (StageResult | undefined)[] =
      stage === "confirm" ? confirmStages(result) : [result.stages[stage]];
    return stages.flatMap((each) => each?.runs ?? []);
  });
}

/** The report's lines of where `stage`'s runs `runs` went, the old text compared with `compared`
 *  (score.ts's `stageArms`): its arms' runs counted by the deployment that answered them
 *  (`<stage> deployments by arm: old <id> <n>, ...; new ...`, gateway.ts's `deploymentWords`),
 *  `not recorded` where no run records its deployments (results made before runs did), with how
 *  many runs do not where some do; and, where an arm's runs went with its `--route-id` pin, the
 *  pins and their cases (`<stage> routing pinned: old=<id>, ...`): such runs are a diagnostic,
 *  never a test result. */
function routingLines(stage: Stage, runs: readonly Run[], compared: Compared): string[] {
  const unrecorded = runs.filter((run) => run.deployments === undefined).length;
  const arms = stageArms(stage, compared);
  const byArm = arms
    .map((arm) => {
      const recorded = runs.flatMap((run) =>
        run.arm === arm && run.deployments !== undefined ? [run.deployments] : []
      );
      return `${arm} ${deploymentWords(recorded)}`;
    })
    .join("; ");
  const words =
    runs.length === 0
      ? "no runs"
      : unrecorded === runs.length
        ? "not recorded"
        : `${byArm}${unrecorded === 0 ? "" : ` (and ${counted(unrecorded, "run")} made before runs recorded their deployments)`}`;
  const lines = [`${stage} deployments by arm: ${words}`];
  const pinned = runs.filter((run) => run.routePinned === true);
  if (pinned.length > 0) {
    const pins = arms.flatMap((arm) => [
      ...new Set(pinned.flatMap((run) => (run.arm === arm ? [`${arm}=${run.routingId}`] : []))),
    ]);
    const cases = [...new Set(pinned.map((run) => run.caseId))];
    lines.push(
      `${stage} routing pinned: ${pins.join(", ")} (${counted(cases.length, "case")}: ${cases.join(", ")}): each run of a pinned arm went with that routing id in every round, not its round's, so these runs are a diagnostic, not a test result`
    );
  }
  return lines;
}

/**
 * The report's lines on the stages `behaviour continue-mixed` wrote (StageResult's `continued`),
 * none where no stage holds it: how many there are, the saved `--out`s they came from, and which
 * a run of the command selected (`--only`; the others' runs were all kept); their runs by stage
 * and arm, each kind apart: continued (a ContinueMixedRun of kind `continued` that does not wait),
 * waiting at the further-round limit (one that waits, `waits`), run again fresh (kind `run
 * again`), the fresh runs made in their place (a run of a later round than the saved stage held,
 * with no ContinueMixedRun), and unchanged (every other run: the saved runs kept as they were);
 * and, apart, the dollars of the continuations (ContinueMixedRun's `usd`: their requests and
 * judgements, a continuation discarded among them) and of the fresh runs (their replays and
 * judgements), each by stage kind. Counts only: they change no verdict.
 */
function continuedLines(results: ReadonlyMap<string, CaseResult>, compared: Compared): string[] {
  type Kind = "continued" | "waiting" | "again" | "fresh" | "unchanged";
  const rows: { stage: Stage; arm: ArmName; kind: Kind; usd: number }[] = [];
  const froms = new Set<string>();
  const selected: string[] = [];
  let stages = 0;
  for (const result of results.values()) {
    for (const place of stagePlaces(result)) {
      const state = place.get(result);
      const continued = state?.continued;
      if (state === undefined || continued === undefined) continue;
      stages += 1;
      froms.add(continued.from);
      if (continued.selected) selected.push(`${result.caseId} ${placeWords(place)}`);
      for (const run of state.runs) {
        const mark = run.continueMixed;
        const kind: Kind =
          mark?.kind === "run again"
            ? "again"
            : mark !== undefined
              ? waits(run)
                ? "waiting"
                : "continued"
              : run.round > continued.savedRounds
                ? "fresh"
                : "unchanged";
        const usd =
          mark?.usd ?? (kind === "fresh" ? usdOf(run.replaySpent) + usdOf(run.judgeSpent) : 0);
        rows.push({ stage: place.stage, arm: run.arm, kind, usd });
      }
    }
  }
  if (stages === 0) return [];
  const byStage = (kind: Kind) =>
    Stage.options
      .map((stage) => {
        const count = (arm: ArmName) =>
          rows.filter((row) => row.stage === stage && row.arm === arm && row.kind === kind).length;
        return `${stage} ${armCounts(stageArms(stage, compared).map((arm) => ({ arm, count: count(arm) })))}`;
      })
      .join("; ");
  const spent = (kinds: readonly Kind[]) => {
    const own = rows.filter((row) => kinds.includes(row.kind));
    const sum = (each: readonly { usd: number }[]) => each.reduce((all, row) => all + row.usd, 0);
    const parts = Stage.options.map(
      (stage) => `${stage} ${dollars(sum(own.filter((row) => row.stage === stage)))}`
    );
    return `${dollars(sum(own))} over ${counted(own.length, "run")} (${parts.join(", ")})`;
  };
  return [
    `continue-mixed: ${counted(stages, "case stage")} written from ${[...froms].join(", ")}, ${selected.length === stages ? "every one selected" : `${selected.length} of them selected by --only${selected.length === 0 ? "" : ` (${selected.join(", ")})`}, the others' runs all kept as saved`}`,
    `continue-mixed continued (a saved run judged at a mixed reply whose other calls only prepare, continued from that reply and judged again), runs by stage and arm: ${byStage("continued")}`,
    `continue-mixed waiting (${FURTHER_ROUND_LIMIT}: a continued run stopped with no verdict at the further-round limit, whose reply would be continued again, which no fresh run replaces until the next round is decided), runs by stage and arm: ${byStage("waiting")}`,
    `continue-mixed run again fresh (a saved run the rule gives no verdict, or whose continuation its record could not rebuild, failed, or another deployment answered), runs by stage and arm: ${byStage("again")}`,
    `continue-mixed fresh runs (made by the stage's extra-run rule in place of the runs with no verdict), runs by stage and arm: ${byStage("fresh")}`,
    `continue-mixed unchanged (saved runs kept as they were), runs by stage and arm: ${byStage("unchanged")}`,
    `continue-mixed spend at list price: the continuations ${spent(["continued", "waiting", "again"])}, their requests and judgements, a discarded one's among them; the fresh runs ${spent(["fresh"])}, their replays and judgements`,
  ];
}

/** `behaviour report`: prints the runners of the results (`runnerLine`, which refuses results of
 *  more than one runner unless told), each case's passes, the skill's verdict and the run's
 *  figures, its spend among them (`spendLines`, and `requestLogLines` for the request log
 *  `requestLog`), `old` being the old text's directory (`--old`), and beside the verdict the lines
 *  of `navigation`, what it read of `--build-summary` (`untestedNavigationLine` and
 *  `replacementLine`; none without the flag). A dropped case's line gives the stage that dropped
 *  it, why, and each stage it ran. Its `outside recorded listing` line counts the runs of the
 *  cases whose capture sends their session's recorded system prompt whole that read a skill
 *  outside that listing (`outsideListing`): a count only, which drops no case and changes no
 *  verdict. Its `deployments by arm` line of each stage kind, and `routing pinned` line of a kind
 *  with pinned runs (`routingLines`), say where the runs went. `continuedLines` counts the
 *  continued runs and their spend by stage without changing the verdict. */
function reportCommand(
  given: Behaviour,
  old: string,
  navigation: BuildNavigation | undefined,
  requestLog: string
): void {
  const results = readResults(given);
  const b = { ...given, compared: recordedCompared(given.out, results) ?? given.compared };
  const runners = runnerLine(b, results);
  const score = scored(b, results);
  const byCase = new Map(score.cases.map((each) => [each.caseId, each]));
  const lines = [
    `skill ${b.cases[0]?.skill}: ${b.cases.length} cases in ${b.casesFile}`,
    runners,
    comparedLine(b, results),
  ];
  for (const c of b.cases) {
    const result = results.get(c.id);
    const drop = byCase.get(c.id)?.drop;
    const ran = result === undefined ? [] : caseStageLines(result, byCase.get(c.id));
    let line =
      drop !== undefined
        ? `dropped at ${drop.stage}: ${drop.reason}; ${ran.length === 0 ? "screen: not run" : ran.join("; ")}`
        : ran.length === 0
          ? "not run"
          : ran.join("; ");
    const removed = result === undefined ? [] : removedReads(result);
    if (removed.length > 0) {
      line += `; ours left out the history's reads of ${removed.join(", ")} (our skill has no file at that path)`;
    }
    lines.push(`case ${c.id}: ${line}`);
  }

  const dropped = [...results.values()].filter((result) => result.dropped !== null);
  const drops = score.cases.flatMap((each) => (each.drop === undefined ? [] : [each.drop.reason]));
  // A case dropped after its judge controls held (its context could not be read) is no drop by
  // them.
  const byControls = dropped.filter((result) => result.controls?.hold === false).length;
  // A drop counts under the kind its reason begins with: one whose listing both misses and adds
  // too many names counts as listing-misses (score.ts's `listingStanding`).
  const droppedAs = (kind: string) =>
    `${drops.filter((reason) => reason.startsWith(kind)).length} ${kind}`;
  const unscreened = score.cases.filter(
    (each) => each.drop === undefined && each.screen.state === "unscreened"
  ).length;
  const flagged = score.cases.filter((each) => each.flagged).length;
  const losses = score.cases.filter((each) => each.loss).length;
  const notRun = b.cases.filter(
    (c) => !results.has(c.id) && byCase.get(c.id)?.drop === undefined
  ).length;
  // Each case dropped for a block its arms cannot rebuild, with the kind of each of its faults.
  const injected = b.cases.flatMap((c) =>
    byCase.get(c.id)?.drop?.reason.startsWith(INJECTION_UNREBUILDABLE) === true
      ? [results.get(c.id)?.injectionKinds ?? injectionDrop(c)?.kinds ?? []]
      : []
  );
  const byKind = InjectionKind.options
    .map((kind) => `${kind} ${injected.filter((kinds) => kinds.includes(kind)).length}`)
    .join(", ");
  // Each case dropped for a file read by path an arm's text holds in a form the arms do not
  // rebuild, by the arm its reason names (`pathFormDrop`): old and new, and ours where it is the
  // compared arm.
  const formed = drops.filter((reason) => reason.startsWith(`${PATH_FORM}: `));
  const byArm = ["old", "new", ...(b.compared === "ours" ? ["ours"] : [])]
    .map(
      (arm) =>
        `${arm} ${formed.filter((reason) => reason.startsWith(`${PATH_FORM}: ${arm}: `)).length}`
    )
    .join(", ");
  lines.push(
    `cases: ${score.screened} screened, ${score.informative} informative, ${flagged} flagged, ${losses} confirmed losses; ${drops.length} dropped (${byControls} by the judge controls, ${droppedAs(MODEL_NOT_SERVED)}, ${droppedAs(UNREADABLE_DIRECTORY)}, ${droppedAs(LEGION_TOOL_UNDEFINED)}, ${droppedAs(LISTING_MISSES)}, ${droppedAs(LISTING_ADDS)}, ${injected.length} ${INJECTION_UNREBUILDABLE} (${byKind}), ${formed.length} ${PATH_FORM} (${byArm}), ${droppedAs(READS_PAST_LAST_ROUND)}, ${droppedAs(PROVIDER_REFUSED)}), ${unscreened} unscreened, ${notRun} not run`
  );
  // Each arm's runs of each stage, the earlier confirms too, whose reply after the last answered
  // round still read (skill files, alone or beside calls that only prepare, or a written case's
  // read-only calls the checkout answers): an arm that keeps the agent reading acts differently.
  const runsByStage = Stage.options.map((stage) => [stage, kindRuns(results, stage)] as const);
  const pastByStage = runsByStage.map(
    ([stage, runs]) => `${stage} ${armCounts(readsPast(runs, stage, b.compared))}`
  );
  lines.push(
    `${READS_PAST_LAST_ROUND}: the runs whose reply after the last of ${MAX_SKILL_ROUNDS} answered rounds of skill reads still read skill files, alone or beside calls that only prepare, or whose reply after the last of ${MAX_CHECKOUT_ROUNDS} rounds of a written case's read-only calls answered from its checkout still only read, which get no verdict and are run again, by stage and arm: ${pastByStage.join("; ")}`
  );
  // Each arm's runs of each stage whose replay met a reply that read skill files beside other
  // calls (replay.ts's `replyBoundary`): continued past one whose other calls only prepare the
  // case's act, judged at one with an act, or left with no verdict by one with an undecided call.
  // One past the last round counts above, as reads-past-last-round.
  const mixedByStage = (counted: (run: Run) => boolean) =>
    runsByStage
      .map(([stage, runs]) => {
        const byArm = stageArms(stage, b.compared).map((arm) => ({
          arm,
          count: runs.filter((run) => run.arm === arm && counted(run)).length,
        }));
        return `${stage} ${armCounts(byArm)}`;
      })
      .join("; ");
  lines.push(
    `mixed replies continued (the reply read skill files beside calls that only prepare the case's act: its reads answered and sent back alone, its other calls left out, never run), runs by stage and arm: ${mixedByStage((run) => (run.leftOut?.length ?? 0) > 0)}`,
    `mixed replies judged (the reply read skill files beside an act, and was judged as it came), runs by stage and arm: ${mixedByStage((run) => run.mixedAct === true)}`,
    `${MIXED_UNDECIDED}: the runs whose reply read skill files beside a call that is neither an act nor only preparation, which get no verdict and are run again, by stage and arm: ${mixedByStage((run) => run.verdict === null && run.reason.startsWith(MIXED_UNDECIDED))}`
  );
  // By kind of case, the runs with no verdict for the reads of a reply, by reason, and the reads
  // a run saved before the runner rebuilt the pin's summaries marks `wholeText`, answered with a
  // file's whole text where the pin may have shown its summary: no run marks one now.
  const byKindOfCase = CaseKind.options.map((kind) => {
    const runs = b.cases
      .filter((c) => caseKind(c) === kind)
      .flatMap((c) => {
        const result = results.get(c.id);
        if (result === undefined) return [];
        return [
          ...Stage.options.flatMap((stage) => result.stages[stage]?.runs ?? []),
          ...(result.earlierConfirms ?? []).flatMap((each) => each.runs),
        ];
      });
    const rerun = (reason: string) =>
      runs.filter((run) => run.verdict === null && run.reason.startsWith(reason)).length;
    const whole = runs
      .flatMap((run) => (run.skillReads ?? []).flat())
      .filter((read) => read.from === "checkout" && read.wholeText === true).length;
    return `${kind}: ${rerun(READS_PAST_LAST_ROUND)} ${READS_PAST_LAST_ROUND}, ${rerun(UNANSWERED_READ)} ${UNANSWERED_READ}, ${rerun(MIXED_UNDECIDED)} ${MIXED_UNDECIDED}, ${whole} saved reads marked wholeText (answered whole before the runner rebuilt the pin's summaries)`;
  });
  lines.push(
    `runs with no verdict, run again, and saved whole-text reads, by kind of case: ${byKindOfCase.join("; ")}`
  );
  // Where each stage kind's runs went: the deployment that answered each arm's runs, and any
  // arm whose runs went with a `--route-id` pin.
  lines.push(...runsByStage.flatMap(([stage, runs]) => routingLines(stage, runs, b.compared)));
  lines.push(...continuedLines(results, b.compared));
  const pooled = score.cases.flatMap(({ caseId, confirm }) =>
    confirm?.state === "counted" && confirm.pooled !== undefined
      ? [
          `${caseId} (confirms of old ${confirm.counted.map((each) => each.old).join(", ")} and ${b.compared} ${confirm.counted.map((each) => each[b.compared] ?? 0).join(", ")} of ${FRESH_RUNS} each; pooled ${pooledWords(confirm.pooled, confirm.loss, b.compared)})`,
        ]
      : []
  );
  if (pooled.length > 0) {
    lines.push(
      `pooled confirms (two confirms at one case's texts on one runner disagreed, so a third ran, and the case is a loss when ${COMPARED_TEXT[b.compared]} passes at least ${LOSS_GAP * POOLED_CONFIRMS} fewer of the ${FRESH_RUNS * POOLED_CONFIRMS} pooled runs than the old text): ${pooled.join("; ")}`
    );
  }
  lines.push(kindLine(b, score));
  lines.push(
    ...screenedKindLines(b, score),
    stepReadsLine(b, score, old),
    meanLine(b, score),
    shapesLine(b, score),
    ...inspectionLines(b, score, results)
  );
  const byControl = ControlName.options.map((name) => {
    const count = dropped.filter((result) => result.controls?.failed.includes(name)).length;
    return `${CONTROLS[name].step} not judged ${CONTROLS[name].wanted} ${count}`;
  });
  lines.push(
    `judge controls failed, by control (a case can fail more than one): ${byControl.join(", ")}`
  );
  lines.push(
    score.controlCase !== undefined
      ? `control case: ${score.controlCase}${replacedWords(score)}`
      : score.noControl !== undefined
        ? `control case: none: ${score.noControl}`
        : "control case: none, since no case is informative"
  );
  const control = score.control;
  if (control?.state === "counted") {
    const { old, minus, twin } = control.passes;
    lines.push(
      `controls, scored by the confirm rule (a loss when the other arm passes at least ${LOSS_GAP} fewer of ${FRESH_RUNS} fresh runs than old): old ${old}/${FRESH_RUNS}, minus ${minus}/${FRESH_RUNS}, twin ${twin}/${FRESH_RUNS}; the planted-loss control (old less minus, ${old - minus}) is ${isControlLoss(control.passes) ? "a loss, as it must be" : "no loss, so the skill gets no verdict"}; the false-loss control (old less twin, the same text twice, ${old - twin}) is ${isFalseLoss(control.passes) ? "a loss, so the skill gets no verdict" : "no loss, as it must be"}`
    );
  } else if (control !== undefined) {
    lines.push(
      `controls: the planted-loss and false-loss controls are ${control.state === "running" ? "not all run (behaviour control)" : "unscreened: an arm lacks its judged runs"}`
    );
  }
  if (b.compared === "ours") lines.push(...oursAgainstOldLines(results, score));
  lines.push(caseModelsLine(b));
  lines.push(...modelLines(b, score), ...storeOnlyLines(b), writtenTasksLine(b, score));
  lines.push(`verdict: ${verdictLine(b, results, score)}`);
  if (navigation !== undefined) {
    lines.push(
      untestedNavigationLine(b, navigation.lines),
      replacementLine(results, navigation.replacements)
    );
  }

  const percent = (x: number) => `${(100 * x).toFixed(2)}%`;
  lines.push(
    b.compared === "none"
      ? "sensitivity of the registered rule: the chance that it calls a loss in one case, by the old text's and no skill's pass rates (exact binomial; with no skill compared, the screen's gap counts once)"
      : `sensitivity of the registered rule: the chance that it calls a loss in one informative case whose no-skill arm passes ${SENSITIVITY.none} of its runs, by the old text's and ${b.compared === "new" ? "the cut's" : "our skill's"} pass rates (exact binomial)`
  );
  for (const old of SENSITIVITY.old) {
    const row = SENSITIVITY.drops.map((drop) => {
      const cut = old - drop;
      const none = b.compared === "none" ? cut : SENSITIVITY.none;
      return `${b.compared} ${cut.toFixed(1)} ${percent(lossChance({ old, compared: cut, none }, b.compared))}`;
    });
    lines.push(`  old ${old.toFixed(1)}: ${row.join(", ")}`);
  }
  lines.push(...dropLines(b, score));

  const chances = score.cases.flatMap((each) => {
    if (!each.informative || each.screen.state !== "counted") return [];
    const { old, none, [b.compared]: other = 0 } = each.screen.passes;
    return [
      { id: each.caseId, chance: falseLossChance({ old, compared: other, none }, b.compared) },
    ];
  });
  const rate = 1 - chances.reduce((none, { chance }) => none * (1 - chance), 1);
  const laplace = `${2 * SCREEN_RUNS + 2}`;
  const how = {
    new: `when the cut's text is the old text; each case's chance is exact binomial given that it is informative, with the old and the cut's texts both passing at (old + new + 1)/${laplace} of their runs and no skill at (none + 1)/${SCREEN_RUNS + 2}`,
    ours: `when our skill's text is the old text; each case's chance is exact binomial given that it is informative, with the old text and ours both passing at (old + ours + 1)/${laplace} of their runs and no skill at (none + 1)/${SCREEN_RUNS + 2}`,
    none: `when no skill passes as the old text does; each case's chance is exact binomial, the screen's gap counted once, with the old text and no skill both passing at (old + none + 1)/${laplace} of their runs`,
  }[b.compared];
  lines.push(
    `false-loss rate at the cases run (an estimate): ${percent(rate)}, the chance that the registered rule calls a loss in one of the ${chances.length} informative cases ${how}, from its screen passes, and the cases are combined as independent, 1 − Π(1 − chance)${chances.length === 0 ? "" : ` (${chances.map(({ id, chance }) => `${id} ${percent(chance)}`).join(", ")})`}`
  );

  const notes = new Map<string, z.infer<typeof CaptureNote>>();
  let peak = 0;
  for (const result of results.values()) {
    peak = Math.max(peak, result.peakRssBytes ?? 0);
    for (const [, state] of eachStage(result)) {
      for (const note of Object.values(state.captures)) notes.set(note.bodyFile, note);
    }
  }
  const differ = [...notes.values()].filter((note) => note.captureDigest !== note.sessionDigest);
  lines.push(
    `digests: ${differ.length} of ${notes.size} captures' system-prompt digests differ from their session's`
  );
  lines.push(judgeLine(results));
  const answers = [...dispatchAnswers(results).values()];
  const from = (answer: DispatchFrom) => answers.filter((each) => each === answer).length;
  lines.push(
    `dispatch: ${answers.length} cases carry the Envoy plugin's block of the skill: ${from("assumed")} rest on the assumption that their session had Dispatch, ${from("legion-role")} take it from a Legion role, ${from("transcript")} from their transcript (arms.ts's dispatchOf)`
  );
  // The runs of each case whose capture sends its session's recorded system prompt whole (its
  // profile holds the prompt, capture.ts's `systemPrompt` choice) that read a skill outside the
  // recorded listing, from the skill reads each run records (a written case's checkout reads
  // aside): a count only, which drops no case and changes no verdict.
  lines.push(
    outsideListingWords(
      outsideListing(
        b.cases.flatMap((c) => {
          const prompt = c.profile.systemPrompt;
          const result = results.get(c.id);
          if (prompt === undefined) return [];
          return [
            {
              recorded: new Set(listingOf(prompt).keys()),
              runs: (result === undefined ? [] : eachStage(result)).flatMap(([, stage]) =>
                stage.runs.map((run) =>
                  run.skillReads?.flat().filter((read) => read.from !== "checkout")
                )
              ),
            },
          ];
        })
      ),
      "case"
    )
  );
  const spend = recordedSpend(results.values());
  lines.push(...spendLines(b, spend), ...requestLogLines(requestLog, b, results, spend));
  lines.push(`peak resident memory: ${mib(peak)} MiB, the most at the end of a case`);
  lines.push(
    b.limits === undefined
      ? "limits: not given (no --limits)"
      : `limits (--limits ${b.limits.file}):\n${b.limits.text.replace(/\n$/, "")}`
  );
  b.io.out(`${lines.join("\n")}\n`);
}

// `behaviour audit-mixed` and `load audit-mixed`: the mixed-reply rule over saved runs.

/** The file `behaviour audit-mixed` and `load audit-mixed` write under `--out`. */
const AUDIT_FILE = "audit-mixed.json";

/** One run an audit names: its number in its arm and stage, its reply's blind id (null when it
 *  was not judged), its session id, its verdict, and the tools of the calls of its judged reply
 *  that put it there (`(text)` for its text). */
interface AuditedRun {
  readonly run: number;
  readonly id: string | null;
  readonly userId: string | null;
  readonly verdict: "pass" | "fail" | null;
  readonly tools: readonly string[];
}

/** Where the installed copies of the profile's other skills an arm's runs are sorted with come
 *  from, as `behaviour audit-mixed` and `behaviour continue-mixed` read them: its stage's saved
 *  capture's record (`capture`, context.ts's `installedCopies`), or, where that capture is not
 *  saved, the case's saved profile (`profile`, `profileInstalledCopies`). */
type CopiesFrom = "capture" | "profile";

/** The installed copies of `arm`'s profile's repository skills (context.ts's `profileCopies`, the
 *  skills a context of the arm installs from their repositories): each under the name the profile
 *  lists it under, from its cache entry `<cacheDir>/skills/<key>`, exactly as `installedCopies`
 *  gives a saved capture record's copies, since a capture's record names those copies. What a
 *  read of another skill is answered from where the arm's capture is not saved. Exported for
 *  cli.test.ts. */
export function profileInstalledCopies(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills">,
  cacheDir: string
): InstalledCopy[] {
  const skills = profileCopies(arm).flatMap(({ record }) => (record === undefined ? [] : [record]));
  return installedCopies({ skills }, cacheDir);
}

/** Why the cache cannot give the profile's installed copies `copies`, or undefined: the first one
 *  whose directory it does not hold, or whose directory cannot be read. */
function unreadableCopy(copies: readonly InstalledCopy[]): string | undefined {
  for (const copy of copies) {
    try {
      const stat = statSync(copy.dir, { throwIfNoEntry: false });
      if (stat === undefined) {
        return `the profile's copy of ${copy.name}, ${copy.dir}, is not in the cache`;
      }
      if (!stat.isDirectory()) {
        return `the profile's copy of ${copy.name}, ${copy.dir}, is not a directory`;
      }
      readdirSync(copy.dir);
    } catch (error) {
      return `the profile's copy of ${copy.name}, ${copy.dir}, cannot be read: ${messageFor(error)}`;
    }
  }
  return undefined;
}

/** The files of the saved capture of case `caseId`'s arm `arm` in stage `state` under `out`: the
 *  directory of the body file its CaptureNote names, else where `armCaptureDir` puts one. */
function stageCaptureFiles(
  out: string,
  caseId: string,
  arm: Arm,
  state: StageResult,
  texts: Texts
): { readonly body: string; readonly record: string } {
  const note = state.captures[arm.name];
  return capturePaths(
    note === undefined
      ? armCaptureDir(out, arm, texts)
      : path.dirname(path.resolve(out, note.bodyFile)),
    caseId,
    arm.name
  );
}

/** What the mixed-reply rule makes of a saved run's judged reply (`sortRun`): no reply saved (a
 *  replay that failed), answered rounds a run saved before `skillReads` cannot tell, a reply that
 *  holds no mixed reply (`ends`) or only reads skill files (`skillRound`); or a mixed reply, with
 *  its answered reads, its other calls sorted and its text's part: an act among them (`acts`),
 *  only preparation (`continues`, or `pastLastRound` after MAX_SKILL_ROUNDS answered rounds), or
 *  an undecided call (`unresolved`). */
type RunSort =
  | { readonly kind: "noReply" }
  | { readonly kind: "unrecorded" }
  | { readonly kind: "ends" }
  | { readonly kind: "skillRound" }
  | { readonly kind: "checkoutRound" }
  | {
      readonly kind: "acts" | "continues" | "pastLastRound" | "unresolved";
      readonly reads: readonly SkillRead[];
      readonly others: readonly SortedCall[];
      readonly text: CallPart | undefined;
    };

/** replay.ts's mixed-reply rule (`replyBoundary` under `rule`) applied to saved run `run`'s judged
 *  reply, its `reply` less its answered rounds' read calls (its first N blocks, N the reads its
 *  `skillReads` holds), for `arm` answering a read of another skill from `copies` (RunSort): the
 *  one sort `behaviour audit-mixed` and `behaviour continue-mixed` apply. Throws as
 *  `replyBoundary` throws, after `label` and the run's number. */
function sortRun(
  run: Run,
  arm: Arm,
  copies: readonly InstalledCopy[],
  rule: MixedRule,
  written: boolean,
  label: string
): RunSort {
  if (run.reply === null) return { kind: "noReply" };
  const answered = answeredCalls(run);
  if (answered === null) return { kind: "unrecorded" };
  let boundary: Boundary;
  try {
    boundary = replyBoundary(run.reply.slice(answered), arm, copies, rule);
  } catch (error) {
    throw new Error(`${label} run ${run.run}: ${messageFor(error)}`);
  }
  if (
    written &&
    boundary.kind !== "ends" &&
    boundary.kind !== "skill round" &&
    toolCalls(run.reply.slice(answered)).every(
      ({ id, name, input }) => id !== undefined && readOnlyCall(name, input)
    )
  ) {
    return { kind: "checkoutRound" };
  }
  switch (boundary.kind) {
    case "ends":
      return { kind: "ends" };
    case "skill round":
      return { kind: "skillRound" };
    case "acts":
      return { ...boundary, kind: "acts" };
    case "undecided":
      return { ...boundary, kind: "unresolved" };
    case "continue":
      return {
        ...boundary,
        kind: (run.skillRounds ?? 0) >= MAX_SKILL_ROUNDS ? "pastLastRound" : "continues",
      };
  }
}

/** What `behaviour audit-mixed` finds of one arm's runs in one stage of a case (see
 *  `auditMixedCommand`). `confirm` numbers confirms from 1, oldest first. `copiesFrom` gives
 *  the source of installed copies. `checkoutRound` counts a written case's replies with both
 *  skill reads and other calls that are all read-only, which go to the checkout first. */
interface ArmAudit {
  readonly caseId: string;
  readonly stage: Stage;
  readonly confirm?: number;
  readonly arm: ArmName;
  readonly copiesFrom: CopiesFrom;
  readonly runs: number;
  readonly judged: number;
  noReply: number;
  unrecorded: number;
  ends: number;
  skillRound: number;
  checkoutRound: number;
  readonly acts: AuditedRun[];
  readonly continues: AuditedRun[];
  readonly pastLastRound: AuditedRun[];
  readonly unresolved: AuditedRun[];
}

/** A stage or a case `behaviour audit-mixed` leaves unaudited, and why: a stage run with other
 *  texts, or a case one of whose arms' copies come from its profile where a copy cannot be read
 *  (with the stage and the arm where it was found). */
interface AuditSkip {
  readonly caseId: string;
  readonly stage: Stage;
  readonly confirm?: number;
  readonly arm?: ArmName;
  readonly why: string;
}

/** An audit's runs in words: their count, and each one's number, blind id (a behaviour run's,
 *  where it has one) and tools. */
function auditedWords(
  runs: readonly {
    readonly run: number;
    readonly id?: string | null;
    readonly tools: readonly string[];
  }[]
): string {
  if (runs.length === 0) return "0";
  const each = runs.map(
    (run) => `run ${run.run}${run.id == null ? "" : ` id ${run.id}`}: ${run.tools.join(" ")}`
  );
  return `${runs.length} (${each.join("; ")})`;
}

/**
 * `behaviour audit-mixed` (no model call, no capture, no network): replay.ts's mixed-reply rule
 * (`sortRun`), applied to the final reply of each saved run, including earlier confirms.
 * Builds the recorded arms from the supplied texts and skips stages whose texts differ.
 * Reads installed copies from the saved capture without restoring them, or from the case's
 * profile if the capture is missing. An unreadable profile copy skips that case. Records counts
 * and identifiers, including which replies go to checkout before the mixed rule, never reply text.
 */
function auditMixedCommand(
  b: Behaviour,
  dirs: ArmDirs,
  texts: Texts,
  cacheDir: string,
  scratchRoot: string
): void {
  const results = readResults(b);
  mkdirSync(scratchRoot, { recursive: true });
  const scratch = mkdtempSync(path.join(scratchRoot, "audit-mixed-"));
  const audits: ArmAudit[] = [];
  const skipped: AuditSkip[] = [];
  /** The audit of each arm of each stage of case `c`'s `result`, or, where an arm's copies come
   *  from its profile and one cannot be read, the skip of the whole case. */
  const auditCase = (c: Case, result: CaseResult): ArmAudit[] | AuditSkip => {
    const found: ArmAudit[] = [];
    const rule: MixedRule = { kind: "case", act: caseAct(c) };
    const armScratch = path.join(
      scratch,
      createHash("sha256").update(c.id).digest("hex").slice(0, 16)
    );
    for (const place of stagePlaces(result)) {
      const state = place.get(result);
      if (state === undefined || state.runs.length === 0) continue;
      const { stage, confirm } = place;
      const where = { caseId: c.id, stage, ...(confirm === undefined ? {} : { confirm }) };
      if (!sameTexts(texts, state)) {
        skipped.push({
          ...where,
          why: `run with the texts ${textWords(state.texts)}, not ${textWords(texts)}`,
        });
        continue;
      }
      const armDirs = {
        ...dirs,
        ...(state.plantedLine === null ? {} : { plantedLine: state.plantedLine }),
      };
      for (const name of stageArms(stage, recordedArm(state))) {
        const runs = state.runs.filter((run) => run.arm === name);
        if (runs.length === 0) continue;
        const arm = buildArm(c, name, armDirs, armScratch);
        const files = stageCaptureFiles(b.out, c.id, arm, state, texts);
        const capture = savedCapture(files);
        const copiesFrom: CopiesFrom = capture === undefined ? "profile" : "capture";
        const copies =
          capture === undefined
            ? profileInstalledCopies(arm, cacheDir)
            : installedCopies(capture.record.context, cacheDir);
        const unreadable = capture === undefined ? unreadableCopy(copies) : undefined;
        if (unreadable !== undefined) {
          return {
            ...where,
            arm: name,
            why: `the ${name} arm's capture of its ${placeWords(place)} is not saved (${files.record}), so its copies come from the case's profile, and ${unreadable}: no stage of the case is audited`,
          };
        }
        const audit: ArmAudit = {
          ...where,
          arm: name,
          copiesFrom,
          runs: runs.length,
          judged: runs.filter((run) => run.verdict !== null).length,
          noReply: 0,
          unrecorded: 0,
          ends: 0,
          skillRound: 0,
          checkoutRound: 0,
          acts: [],
          continues: [],
          pastLastRound: [],
          unresolved: [],
        };
        for (const run of runs) {
          const sorted = sortRun(
            run,
            arm,
            copies,
            rule,
            caseKind(c) === "written",
            `case ${c.id} ${stage} ${name}`
          );
          if (sorted.kind === "noReply") audit.noReply += 1;
          else if (sorted.kind === "unrecorded") audit.unrecorded += 1;
          else if (sorted.kind === "ends") audit.ends += 1;
          else if (sorted.kind === "skillRound") audit.skillRound += 1;
          else if (sorted.kind === "checkoutRound") audit.checkoutRound += 1;
          else {
            const part: CallPart =
              sorted.kind === "acts"
                ? "act"
                : sorted.kind === "unresolved"
                  ? "undecided"
                  : "preparation";
            audit[sorted.kind].push({
              run: run.run,
              id: run.id,
              userId: run.userId,
              verdict: run.verdict,
              tools: [
                ...sorted.others.flatMap((call) => (call.part === part ? [call.tool] : [])),
                ...(sorted.text === part ? ["(text)"] : []),
              ],
            });
          }
        }
        found.push(audit);
      }
    }
    return found;
  };
  try {
    for (const c of b.cases) {
      const result = results.get(c.id);
      if (result === undefined) continue;
      const audited = auditCase(c, result);
      if (Array.isArray(audited)) audits.push(...audited);
      else skipped.push(audited);
    }
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
  const file = path.join(b.out, AUDIT_FILE);
  const sum = (count: (audit: ArmAudit) => number) =>
    audits.reduce((total, audit) => total + count(audit), 0);
  const totals = {
    runs: sum((audit) => audit.runs),
    judged: sum((audit) => audit.judged),
    acts: sum((audit) => audit.acts.length),
    continues: sum((audit) => audit.continues.length),
    pastLastRound: sum((audit) => audit.pastLastRound.length),
    unresolved: sum((audit) => audit.unresolved.length),
  };
  for (const audit of audits) {
    const unrecorded =
      audit.unrecorded === 0 ? "" : `; answered rounds not recorded ${audit.unrecorded}`;
    const checkout =
      audit.checkoutRound === 0
        ? ""
        : `; a written case's read-only calls, a round of its checkout ${audit.checkoutRound}`;
    const copies =
      audit.copiesFrom === "capture"
        ? "copies from the saved capture"
        : "copies from the case's profile, its capture not saved";
    say(
      b,
      `case ${audit.caseId} ${placeWords(audit)} ${audit.arm}: ${audit.runs} runs, ${audit.judged} judged; judged at a mixed reply with an act ${auditedWords(audit.acts)}; continued now ${auditedWords(audit.continues)}; no verdict past the last round ${auditedWords(audit.pastLastRound)}; unresolved ${auditedWords(audit.unresolved)}; only skill reads ${audit.skillRound}; no mixed reply ${audit.ends}; no reply ${audit.noReply}${checkout}${unrecorded}; ${copies}`
    );
  }
  for (const each of skipped) {
    const arm = each.arm === undefined ? "" : ` ${each.arm}`;
    say(b, `case ${each.caseId} ${placeWords(each)}${arm}: skipped: ${each.why}`);
  }
  writeWhole(
    file,
    json({
      cases: b.casesFile,
      out: b.out,
      texts,
      maxSkillRounds: MAX_SKILL_ROUNDS,
      totals,
      arms: audits,
      skipped,
    })
  );
  say(
    b,
    `${totals.runs} runs, ${totals.judged} judged: ${totals.acts} judged at a mixed reply with an act, ${totals.continues} continued now, ${totals.pastLastRound} with no verdict past the last round, ${totals.unresolved} unresolved; written to ${file}`
  );
}

// `behaviour continue-mixed`: the saved runs the mixed-reply rule continues, continued.

/** How the reason begins of a saved run `behaviour continue-mixed` runs again fresh because its
 *  record lacks a part its continuation needs, or because another deployment than the one that
 *  answered its saved reply answered its continuation. */
const CONTINUE_MIXED = "continue-mixed";

/** The file `behaviour continue-mixed --probe` writes under `--out`. */
const PROBE_FILE = "probe.json";

/** A saved `--out`'s request log as continue-mixed reads it (`readSavedLog`): its path, its
 *  SHA-256, and the deployments that answered the logged requests of each session id. */
interface SavedLog {
  readonly file: string;
  readonly sha256: string;
  readonly byUser: ReadonlyMap<string, ReadonlySet<string>>;
}

/** The request log `file` (`loggedGateway`'s lines), or undefined where it does not exist: under
 *  the session id (`userId`) of each line that holds the gateway's headers of its answer (`gateway`,
 *  a request that got one), the deployment that answered it (gateway.ts's `deploymentOf` of its
 *  `x-litellm-model-id`; "" where it names none). A line that is no JSON, or that names no session
 *  id or no answer, is left out. */
function readSavedLog(file: string): SavedLog | undefined {
  if (!existsSync(file)) return undefined;
  const byUser = new Map<string, Set<string>>();
  for (const { value } of jsonLines(file)) {
    const userId = field(value, "userId");
    const headers = field(value, "gateway");
    if (typeof userId !== "string" || typeof headers !== "object" || headers === null) continue;
    const model = field(headers, "x-litellm-model-id");
    const deployment = deploymentOf({
      gatewayHeaders: typeof model === "string" ? { "x-litellm-model-id": model } : {},
    });
    const each = byUser.get(userId) ?? new Set<string>();
    each.add(deployment);
    byUser.set(userId, each);
  }
  const sha256 = createHash("sha256").update(readFileSync(file)).digest("hex");
  return { file, sha256, byUser };
}

/** The deployment that answered saved run `run`'s saved reply (SavedDeployment), or why none can
 *  be taken: its last `deployments` entry, where it records them; else (a run saved before runs
 *  recorded them) the one deployment that answered every logged request of its session id in its
 *  saved `--out`'s request log (`log()`, read from `logFile`). A log that is missing, that holds
 *  no answered request of that id, or that names more than one deployment for it, or none, gives
 *  none: a per-arm `--route-id` is never read for one, since the arms of one stage were split
 *  across deployments. */
function savedDeployment(
  run: Run,
  log: () => SavedLog | undefined,
  logFile: string
): SavedDeployment | { readonly missing: string } {
  if (run.deployments !== undefined) {
    const last = run.deployments.at(-1);
    return last === undefined || last === ""
      ? { missing: "the deployment that answered its saved reply, which its record does not name" }
      : { id: last, from: "run" };
  }
  const saved = log();
  if (saved === undefined) {
    return {
      missing: `the deployment that answered its saved reply: it was saved before runs recorded one, and the saved --out holds no request log (${logFile})`,
    };
  }
  const found = [...(run.userId === null ? [] : (saved.byUser.get(run.userId) ?? []))];
  const [only] = found;
  if (found.length === 1 && only !== undefined && only !== "") {
    return { id: only, from: "request log", file: saved.file, sha256: saved.sha256 };
  }
  return {
    missing:
      found.length === 0
        ? `the deployment that answered its saved reply: it was saved before runs recorded one, and the request log ${saved.file} holds no answered request of its session id`
        : `the deployment that answered its saved reply: it was saved before runs recorded one, and the request log ${saved.file} names ${found.map((each) => each || "no deployment").join(" and ")} for its session id's requests, not one`,
  };
}

/** What one continuation's requests got, each answered one in the order sent: its provider
 *  request id (gateway.ts's Reply `providerRequestId`, null where the answer carried none), the
 *  deployment that answered it (`deploymentOf`), and what it used. */
interface ContinuationCalls {
  readonly requestIds: (string | null)[];
  readonly deployments: string[];
  readonly spent: Spent[];
}

/** What a continuation rejects with when another deployment than `saved`, the one that answered
 *  the run's saved reply, answered one of its requests (`pinnedTo`). */
class OtherDeployment extends Error {
  constructor(
    readonly answered: string,
    readonly saved: string
  ) {
    super(
      `its continuation was answered by deployment ${answered === "" ? "none named" : answered}, not ${saved}, the one that answered its saved reply`
    );
  }
}

/** `gateway` for one continuation: each answer is recorded in `calls`, then rejected with
 *  OtherDeployment where the deployment that gave it (`deploymentOf`, its `x-litellm-model-id`) is
 *  not `deployment`, so a continuation answered elsewhere goes no further. */
function pinnedTo(gateway: Gateway, deployment: string, calls: ContinuationCalls): Gateway {
  return {
    async send(body, options) {
      const reply = await gateway.send(body, options);
      const answered = deploymentOf(reply);
      calls.requestIds.push(reply.providerRequestId);
      calls.deployments.push(answered);
      calls.spent.push(reply.spent);
      if (answered !== deployment) throw new OtherDeployment(answered, deployment);
      return reply;
    },
    spend: () => gateway.spend(),
  };
}

/** One arm of a saved stage as continue-mixed reads it (`savedArm`): the arm, the files of its
 *  stage's saved capture and that capture (undefined where it is not saved), and the installed
 *  copies its runs are sorted and answered with, and where they came from. */
interface SavedArm {
  readonly arm: Arm;
  readonly files: { readonly body: string; readonly record: string };
  readonly capture: Capture | undefined;
  readonly copies: readonly InstalledCopy[];
  readonly copiesFrom: CopiesFrom;
}

/** The arms of `stage` of case `c` as the saved stage `state` under `from` ran them: each built
 *  from `r.dirs` with the stage's own planted line in `scratch` (as runStage builds them), with its
 *  saved capture (`restoredCapture`), whose installed copies are made again where the cache
 *  lacks them (it refuses a capture whose record names other copies
 *  than the profile now gives), or, where the capture is not saved, the copies of the case's
 *  profile (`profileInstalledCopies`); or why the case cannot be continued, a profile copy the
 *  cache does not hold or cannot read. */
async function savedArms(
  r: Runner,
  c: Case,
  stage: Stage,
  state: StageResult,
  from: string,
  scratch: string
): Promise<Map<ArmName, SavedArm> | { readonly unreadable: string }> {
  const { plantedLine: _givenPlantedLine, ...skillDirs } = r.dirs;
  const dirs = {
    ...skillDirs,
    ...(state.plantedLine === null ? {} : { plantedLine: state.plantedLine }),
  };
  const arms = new Map<ArmName, SavedArm>();
  for (const name of stageArms(stage, recordedArm(state))) {
    const arm = buildArm(c, name, dirs, scratch);
    const files = stageCaptureFiles(from, c.id, arm, state, r.texts);
    const capture = await restoredCapture(c, arm, files, {
      source: r.source,
      cacheDir: r.cacheDir,
    });
    if (capture !== undefined) {
      const copies = installedCopies(capture.record.context, r.cacheDir);
      arms.set(name, { arm, files, capture, copies, copiesFrom: "capture" });
      continue;
    }
    const copies = profileInstalledCopies(arm, r.cacheDir);
    const unreadable = unreadableCopy(copies);
    if (unreadable !== undefined) {
      return {
        unreadable: `the ${name} arm's capture is not saved (${files.record}), so its copies come from the case's profile, and ${unreadable}`,
      };
    }
    arms.set(name, { arm, files, capture, copies, copiesFrom: "profile" });
  }
  return arms;
}

/** What `behaviour continue-mixed` does with one saved run (`planRun`): leaves it as saved
 *  (`keep`, and why); continues it from its saved reply (`continue`, with the deployment that
 *  answered that reply); or gives it no verdict, so its stage's extra-run rule runs it again fresh
 *  (`again`, with the reason it then holds, and the deployment taken for it, where one was). */
type RunPlan =
  | { readonly kind: "keep"; readonly why: string }
  | { readonly kind: "continue"; readonly deployment: SavedDeployment }
  | { readonly kind: "again"; readonly reason: string };

/** What continue-mixed does with saved run `run`, which the rule sorts `sorted` (`sortRun`) in an
 *  arm whose saved capture is `saved.capture` (RunPlan): a run it made already, and a run with no
 *  verdict (its stage ran it again already, live), are kept; a judged run whose reply the rule
 *  continues needs the whole earlier answered replies, if any, a session id, the saved capture,
 *  and the deployment that answered its last saved reply (`deployment()`).
 *  and else run again fresh under CONTINUE_MIXED, naming each part it lacks; a judged run past the
 *  last round gets no verdict as READS_PAST_LAST_ROUND, and an undecided one as MIXED_UNDECIDED,
 *  each run again fresh; every other run is kept. */
function planRun(
  run: Run,
  sorted: RunSort,
  saved: SavedArm,
  deployment: () => SavedDeployment | { readonly missing: string }
): RunPlan {
  if (run.continueMixed !== undefined) {
    return { kind: "keep", why: `continue-mixed made it already (${run.continueMixed.kind})` };
  }
  if (run.verdict === null) {
    return { kind: "keep", why: "it has no verdict, so its stage ran it again already" };
  }
  if (
    sorted.kind !== "continues" &&
    sorted.kind !== "pastLastRound" &&
    sorted.kind !== "unresolved"
  ) {
    return { kind: "keep", why: `the rule leaves it as it was (${sorted.kind})` };
  }
  const files = sorted.reads.map(({ read }) => `${read.skill} ${read.file}`).join(", ");
  if (sorted.kind === "unresolved") {
    const what = [
      ...sorted.others.flatMap((call) => (call.part === "undecided" ? [call.tool] : [])),
      ...(sorted.text === "undecided" ? ["its text"] : []),
    ].join(", ");
    return {
      kind: "again",
      reason: `${MIXED_UNDECIDED}: its saved reply, judged before the mixed-reply rule, reads skill files (${files}) beside what the rule sorts neither as an act nor as preparation (${what}), so it gets no verdict and is run again fresh`,
    };
  }
  const tools = sorted.others.map((call) => call.tool).join(", ");
  if (sorted.kind === "pastLastRound") {
    return {
      kind: "again",
      reason: `${READS_PAST_LAST_ROUND}: after ${run.skillRounds} answered rounds of skill reads, its saved reply, judged before the mixed-reply rule, still reads skill files (${files}) beside calls that only prepare (${tools}), so it gets no verdict`,
    };
  }
  const prior = priorRounds(run);
  const rebuildable = prior === 0 || (prior !== null && run.answeredReplies?.length === prior);
  const found = rebuildable && run.userId !== null ? deployment() : undefined;
  const lacks = [
    ...(rebuildable
      ? []
      : [
          prior === null
            ? "the number of its earlier answered rounds"
            : `its saved reply comes after ${prior} answered rounds, whose thinking and text its saved step drops`,
        ]),
    ...(run.userId === null ? ["a session id"] : []),
    ...(saved.capture === undefined ? [`the arm's saved capture (${saved.files.body})`] : []),
    ...(found !== undefined && "missing" in found ? [found.missing] : []),
  ];
  if (found !== undefined && !("missing" in found) && lacks.length === 0) {
    return { kind: "continue", deployment: found };
  }
  return {
    kind: "again",
    reason: `${CONTINUE_MIXED}: the rule continues its saved reply, and its record lacks a part its next request needs (${lacks.join("; ")}), so it is run again fresh`,
  };
}

/** Saved run `run` of stage `state` as a ContinueMixedRun names it: its number, blind id, round,
 *  verdict, and the runner that made it (its own, else its stage's, else UNRECORDED_RUNNER). */
function savedRunNote(run: Run, state: StageResult): ContinueMixedRun["saved"] {
  return {
    run: run.run,
    id: run.id,
    round: run.round,
    verdict: run.verdict,
    runner: run.runner ?? state.runner ?? UNRECORDED_RUNNER,
  };
}

/** What continue-mixed continues each stage with (`continueStage`): the saved `--out` it reads
 *  (`from`); the gateway its continuations go through (`continuations`, logged as
 *  `continuation`); the saved request log (`log()`, read from `logFile`), from which a run saved
 *  before runs recorded their deployments takes the one that answered it; the stages `--only`
 *  selects, each `<case id>\t<stage>` (`placeWords`; undefined: every stage); and the most further
 *  rounds a continuation sends (`--further-rounds`; undefined: the live rule's). */
interface ContinueOptions {
  readonly from: string;
  readonly continuations: Gateway;
  readonly log: () => SavedLog | undefined;
  readonly logFile: string;
  readonly only: ReadonlySet<string> | undefined;
  readonly furtherRounds: number | undefined;
}

/**
 * Continues the saved stage at `place` of case `c`'s `result` (see `continueMixedCommand`). A
 * stage `o.only` does not select is written as saved, every run of it kept, and marked so
 * (`continued.selected` false). In a selected one each saved run is planned (`planRun`, after
 * `sortRun`); the runs to continue are resumed from their saved replies (replay.ts's
 * `resumeReplay`, the live replay's own code, with each arm's saved capture's body and headers,
 * the run's own session id and routing id, the installed copies, and `o.furtherRounds`), all at
 * once in one order seeded by a fresh seed recorded as the stage's next round (`sendOrder`;
 * `o.continuations`, whose calls the gateway bounds), each request through `pinnedTo` the
 * deployment that answered the saved reply; and the continued replies are judged blind with the
 * case's judge under that seed (`judgeBlind`). A continued run takes its saved run's place and
 * number, in the new round, with the continuation's step, reads, left-out calls and verdict, the
 * saved run's spends and deployments then the continuation's, this runner, and its
 * ContinueMixedRun; a continuation the rule leaves with no verdict (ReadsPastLastRound,
 * UndecidedReply) is a continued run with no verdict, as a live run would be, and one stopped at
 * its further rounds (FurtherRoundLimit) a continued run that waits (score.ts's
 * FURTHER_ROUND_LIMIT). A continuation a request of which another deployment answered, or that
 * failed (its reason `<REPLAY_FAILED>: <why>`, as a live replay's), and a run planned `again`,
 * keep the saved run with no verdict and their reason, the calls the continuation got added. A
 * continuation or a judgement the budget refused leaves its saved run as it was. The stage is
 * then written in its place with the new seed, its runs, its spend, `continued`, and the state its
 * runs now give (score.ts's `stageOutcome`; one left unscreened drops the case as runStage drops
 * one, `readsPastDrop`, then `refusedDrop`); where the budget refused a call it throws Stopped
 * after the write, so a command started again goes on from it. Where an arm then lacks runs it can
 * start, runStage runs them, fresh, by the stage's extra-run rule, which replaces no run that waits
 * (its captures copied to `r.out` before, `copyCaptures`). `arms` are the stage's arms as
 * `savedArms` read them, which `continueMixedCommand` reads for every stage it continues before
 * it writes any of the case. Gives the result, and says what it did.
 */
async function continueStage(
  r: Runner,
  c: Case,
  place: StagePlace,
  result: CaseResult,
  phases: Phases,
  o: ContinueOptions,
  arms: ReadonlyMap<ArmName, SavedArm> | undefined
): Promise<CaseResult> {
  const state = place.get(result);
  const where = `case ${c.id} ${placeWords(place)}`;
  if (state === undefined || state.runs.length === 0) return result;
  if (!sameTexts(r.texts, state)) {
    say(
      r,
      `${where}: left as saved: run with the texts ${textWords(state.texts)}, not ${textWords(r.texts)}`
    );
    return result;
  }
  const marked = state.continued ?? {
    from: path.resolve(o.from),
    savedRounds: state.seeds.length,
    selected: false,
  };
  if (!selectedBy(o, c, place)) {
    const current = place.put(result, { ...state, continued: marked });
    const file = writeResult(r, current, phases);
    say(r, `${where}: not selected by --only, every run left as saved; written to ${file}`);
    return current;
  }
  if (arms === undefined) throw new Error(`${where}: its arms were not read`);
  const { stage } = place;
  const rule: MixedRule = { kind: "case", act: caseAct(c) };
  const plans = state.runs.map((run): RunPlan => {
    const saved = arms.get(run.arm);
    if (saved === undefined) throw new Error(`${where}: no arm ${run.arm} in the stage`);
    const sorted = sortRun(
      run,
      saved.arm,
      saved.copies,
      rule,
      caseKind(c) === "written",
      `${where} ${run.arm}`
    );
    return planRun(run, sorted, saved, () => savedDeployment(run, o.log, o.logFile));
  });

  // The continuations, sent at once in one seeded order of their arms' runs.
  const seed = newSeed();
  const round = state.seeds.length + 1;
  const byArm = new Map<ArmName, { at: number; deployment: SavedDeployment }[]>();
  for (const [at, plan] of plans.entries()) {
    const run = state.runs[at];
    if (plan.kind !== "continue" || run === undefined) continue;
    byArm.set(run.arm, [...(byArm.get(run.arm) ?? []), { at, deployment: plan.deployment }]);
  }
  const order = sendOrder(
    [...byArm].map(([key, each]) => ({ key, runs: each.length })),
    seed
  ).flatMap(({ key, run }, sendPlace) => {
    const each = byArm.get(key)?.[run];
    return each === undefined ? [] : [{ ...each, sendPlace }];
  });
  const prepared = order.length === 0 ? {} : await continuationCheckout(r, c);
  const spentBefore = r.gateway.spend().usd;
  const replaysStarted = performance.now();
  const sent = order.map(({ at, deployment, sendPlace }) => {
    const run = state.runs[at] as Run;
    const { arm, capture, copies } = arms.get(run.arm) as SavedArm;
    if (capture === undefined) throw new Error(`${where}: run ${run.run} has no capture`);
    const calls: ContinuationCalls = { requestIds: [], deployments: [], spent: [] };
    const replayed = resumeReplay(
      capture.body,
      savedReplyOf(run),
      arm,
      pinnedTo(o.continuations, deployment.id, calls),
      {
        headers: capture.record.requestHeaders,
        copies,
        mixed: rule,
        ...(prepared.checkout === undefined ? {} : { checkout: prepared.checkout }),
        ...(o.furtherRounds === undefined ? {} : { furtherRounds: o.furtherRounds }),
      }
    );
    return { at, run, deployment, sendPlace, calls, replayed };
  });
  const outcomes = await Promise.allSettled(sent.map((each) => each.replayed));
  const judgeStarted = performance.now();
  phases.replaysMs += judgeStarted - replaysStarted;
  const replies: ArmReply[] = sent.flatMap(({ run }, index) => {
    const outcome = outcomes[index];
    return outcome?.status === "fulfilled"
      ? [{ arm: run.arm, run: run.run, content: outcome.value.step.message.content }]
      : [];
  });
  const judged =
    replies.length === 0 ? [] : (await judgeBlind(c, replies, r.judge, seed, blinded(r, c))).judged;
  phases.judgeMs += performance.now() - judgeStarted;
  const judgements = new Map(judged.map((each) => [`${each.arm}:${each.run}`, each]));

  const runs = [...state.runs];
  let budgetSpent = false;
  /** Saved run `run` with no verdict, `reason` its reason, run again fresh. */
  const again = (
    run: Run,
    reason: string,
    deployment: SavedDeployment | null,
    calls: ContinuationCalls
  ): Run => ({
    ...run,
    verdict: null,
    reason,
    id: null,
    replaySpent: [...run.replaySpent, ...calls.spent],
    ...(run.deployments === undefined && deployment === null
      ? {}
      : {
          deployments: [
            ...(run.deployments ?? (deployment === null ? [] : [deployment.id])),
            ...calls.deployments,
          ],
        }),
    continueMixed: {
      kind: "run again",
      saved: savedRunNote(run, state),
      runner: r.runner,
      deployment,
      requestIds: [...calls.requestIds],
      usd: usdOf(calls.spent),
    },
  });
  for (const [at, plan] of plans.entries()) {
    const run = state.runs[at];
    if (run !== undefined && plan.kind === "again") {
      runs[at] = again(run, plan.reason, null, { requestIds: [], deployments: [], spent: [] });
    }
  }
  for (const [index, each] of sent.entries()) {
    const outcome = outcomes[index] as PromiseSettledResult<Replayed>;
    if (outcome.status === "rejected") {
      const error: unknown = outcome.reason;
      if (error instanceof BudgetSpent) {
        budgetSpent = true;
        continue;
      }
      const stopped =
        error instanceof ReadsPastLastRound ||
        error instanceof UndecidedReply ||
        error instanceof UnansweredRead ||
        error instanceof FurtherRoundLimit
          ? error
          : undefined;
      runs[each.at] =
        stopped === undefined
          ? again(
              each.run,
              error instanceof OtherDeployment
                ? `${CONTINUE_MIXED}: ${error.message}, so it is run again fresh`
                : error instanceof NotResumable
                  ? `${CONTINUE_MIXED}: its record could not rebuild the next request (${error.message}), so it is run again fresh`
                  : `${REPLAY_FAILED}: ${messageFor(error)}`,
              each.deployment,
              each.calls
            )
          : continuedRun(r, state, round, each, stopped.replayed, {
              verdict: null,
              reason: stopped.message,
              id: null,
              spent: [],
            });
      continue;
    }
    const judgement = judgements.get(`${each.run.arm}:${each.run.run}`);
    if (judgement === undefined || judgement.budgetRefused) {
      budgetSpent = true;
      continue;
    }
    runs[each.at] = continuedRun(r, state, round, each, outcome.value, {
      verdict: judgement.verdict,
      reason: judgement.reason,
      id: judgement.id,
      spent: [...judgement.spent],
    });
  }
  // The stage's state as its runs now stand; a stage left unscreened drops its case as runStage
  // would, and only a stage with runs to start needs runStage and its captures.
  const compared = recordedArm(state);
  const standing = stageOutcome(runs, c.id, stage, compared);
  const toStart =
    standing.state === "running" && Object.values(standing.toStart).some((count) => count > 0);
  const next: StageResult = {
    ...state,
    ...(prepared.record === undefined ? {} : { checkout: prepared.record }),
    state: standing.state,
    seeds: order.length === 0 ? state.seeds : [...state.seeds, seed],
    runs,
    usd: state.usd + r.gateway.spend().usd - spentBefore,
    continued: { ...marked, selected: true },
  };
  const dropped =
    standing.state === "unscreened"
      ? (readsPastDrop(runs, stage, compared) ?? refusedDrop(runs, stage, compared))
      : undefined;
  let current = place.put(result, next);
  if (dropped !== undefined) current = { ...current, dropped, droppedAt: stage };
  const file = writeResult(r, current, phases);
  // The runs it changed, in the stage's order, by what it made of each.
  const changed = runs.filter((run, at) => run !== state.runs[at]);
  const of = (kind: "continued" | "waiting" | "run again") => {
    const own = changed.filter((run) =>
      kind === "run again"
        ? run.continueMixed?.kind === "run again"
        : run.continueMixed?.kind === "continued" && waits(run) === (kind === "waiting")
    );
    const each = own.map((run) => `${run.arm} run ${run.run}`).join(", ");
    const what =
      kind === "waiting"
        ? "waiting at the further-round limit"
        : kind === "run again"
          ? "run again fresh"
          : "continued";
    return `${own.length} ${what}${own.length === 0 ? "" : ` (${each})`}`;
  };
  say(
    r,
    `${where}: continue-mixed: ${of("continued")}, ${of("waiting")}, ${of("run again")}, ${runs.length - changed.length} left as saved; written to ${file}`
  );
  if (budgetSpent) {
    throw new Stopped(
      `the budget is spent: the replies so far cost $${r.gateway.spend().usd.toFixed(4)} at list price, so no new model call starts; ${where}'s continuations so far are saved in ${file}, and a continue-mixed started again goes on from them`
    );
  }
  if (toStart) {
    copyCaptures(r, c, stage, next, arms, o.from);
    const { plantedLine: _givenPlantedLine, ...skillDirs } = r.dirs;
    const stageRunner: Runner = {
      ...r,
      dirs: {
        ...skillDirs,
        ...(next.plantedLine === null ? {} : { plantedLine: next.plantedLine }),
      },
    };
    current = await runStage(stageRunner, c, stage, current, phases, place);
  }
  return current;
}

/** Saved run `each.run` of stage `state`, continued (see `continueStage`): in its place and
 *  number, in round `round` at `each.sendPlace`, with the continuation's step `replayed`, the
 *  calls it got (`each.calls`), the deployment that answered its saved reply
 *  (`each.deployment`), and its judgement. */
function continuedRun(
  r: Runner,
  state: StageResult,
  round: number,
  each: {
    readonly run: Run;
    readonly sendPlace: number;
    readonly deployment: SavedDeployment;
    readonly calls: ContinuationCalls;
  },
  replayed: Replayed,
  judgement: { verdict: "pass" | "fail" | null; reason: string; id: string | null; spent: Spent[] }
): Run {
  const { run, sendPlace, deployment, calls } = each;
  return {
    caseId: run.caseId,
    arm: run.arm,
    stage: run.stage,
    run: run.run,
    round,
    sendPlace,
    userId: run.userId,
    ...(run.routingId === undefined ? {} : { routingId: run.routingId }),
    ...(run.routePinned === true ? { routePinned: true as const } : {}),
    verdict: judgement.verdict,
    reason: judgement.reason,
    id: judgement.id,
    skillRounds: replayed.rounds,
    checkoutRounds: replayed.checkoutRounds,
    ...(replayed.answeredReplies.length === 0
      ? {}
      : { answeredReplies: replayed.answeredReplies.map((each) => [...each]) }),
    skillReads: replayed.reads.map((each) => [...each]),
    leftOut: [...replayed.leftOut],
    ...(replayed.mixedAct ? { mixedAct: true as const } : {}),
    reply: replayed.step.message.content,
    replaySpent: [...run.replaySpent, ...calls.spent],
    deployments: [...(run.deployments ?? [deployment.id]), ...calls.deployments],
    judgeSpent: [...run.judgeSpent, ...judgement.spent],
    runner: r.runner,
    continueMixed: {
      kind: "continued",
      saved: savedRunNote(run, state),
      runner: r.runner,
      deployment,
      requestIds: [...calls.requestIds],
      usd: usdOf([...calls.spent, ...judgement.spent]),
    },
  };
}

/** Copies to `r.out` each saved capture (its body and its record) of the arms `arms` of case
 *  `c`'s `stage` under `from`, where runStage reads a capture back (`armCaptureDir` under `r.out`,
 *  at the stage's texts), unless one is there already: the fresh runs of a continued stage, and
 *  the stage commands later run on `r.out`, replay the captures its saved runs replayed. The saved
 *  `--out` is only read. */
function copyCaptures(
  r: Runner,
  c: Case,
  stage: Stage,
  state: StageResult,
  arms: ReadonlyMap<ArmName, SavedArm>,
  from: string
): void {
  for (const name of stageArms(stage, recordedArm(state))) {
    const saved = arms.get(name);
    if (saved?.capture === undefined) continue;
    const target = capturePaths(armCaptureDir(r.out, saved.arm, state.texts), c.id, name);
    for (const part of ["body", "record"] as const) {
      const inside = path.resolve(saved.files[part]).startsWith(path.resolve(from));
      if (existsSync(target[part]) || !inside) continue;
      mkdirSync(path.dirname(target[part]), { recursive: true });
      copyFileSync(saved.files[part], target[part]);
    }
  }
}

/** Whether the saved stage ran the supplied texts. */
function sameTexts(texts: Texts, state: StageResult): boolean {
  return (
    state.texts.old === texts.old &&
    state.texts.new === texts.new &&
    state.texts.ours === texts.ours
  );
}

function priorRounds(run: Run): number | null {
  if (run.skillRounds === null || run.checkoutRounds === null) return null;
  return Math.max(run.skillRounds + (run.checkoutRounds ?? 0), run.skillReads?.length ?? 0);
}

/** How many answered calls precede the final reply in the persisted judged step. */
function answeredCalls(run: Run): number | null {
  return (
    run.skillReads?.reduce((total, round) => total + round.length, 0) ??
    (priorRounds(run) === 0 ? 0 : null)
  );
}

/** The whole final reply, apart from the judged step's prefix of answered calls. */
function savedReplyOf(run: Run): SavedReply {
  return {
    userId: run.userId,
    routingId: run.routingId ?? null,
    content: run.reply?.slice(answeredCalls(run) ?? 0) ?? [],
    ...(run.answeredReplies === undefined ? {} : { answeredReplies: run.answeredReplies }),
  };
}

/** Whether `--only` (`o.only`) selects case `c`'s stage at `place`: every stage without it. */
function selectedBy(o: Pick<ContinueOptions, "only">, c: Case, place: StagePlace): boolean {
  return o.only === undefined || o.only.has(`${c.id}\t${placeWords(place)}`);
}

/** The stages `--only <file>` selects: each line, blank lines aside, `<case id>`, a tab, and
 *  `screen`, `control` or `confirm <n>` (a case's confirms numbered from 1, oldest first, as
 *  `behaviour audit-mixed` numbers them), each as `<case id>\t<stage>`. Refuses a line of another
 *  form, naming the file and the line. */
function readOnly(file: string): Set<string> {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`--only ${file} cannot be read: ${messageFor(error)}`);
  }
  const selected = new Set<string>();
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    const tab = line.indexOf("\t");
    const stage = line.slice(tab + 1).trim();
    if (tab <= 0 || !/^(screen|control|confirm [1-9][0-9]*)$/.test(stage)) {
      throw new Refused(
        `--only ${file}:${index + 1} is not <case id>, a tab, and screen, control or confirm <n>: ${JSON.stringify(line)}`
      );
    }
    selected.add(`${line.slice(0, tab)}\t${stage}`);
  }
  return selected;
}

/**
 * `behaviour continue-mixed`: the cut review's rule counts a saved case only once the saved runs the
 * mixed-reply rule continues are continued, and this does it, from the saved `--out` `from`,
 * which it only reads, into `r.out`. Before any model call it refuses a saved stage (or one it
 * wrote in `r.out` before) of a runner that is neither this one nor one `--allow-runners` names
 * (`refuseOtherRunners`), and an `--only` (`onlyFile`, `readOnly`) that names a case the file
 * lacks or a stage its result does not hold. Then per case in the file's order: a case with no
 * saved result is skipped, and a dropped one (`caseDrop`) is written as saved; any other gets,
 * for each stage its saved result holds (screen, each confirm, control), the stage
 * `continueStage` makes (only the stages `--only` names are continued, the others written as
 * saved; a continuation sends at most `furtherRounds` requests where given), its result written
 * to `r.out` (`finishCase`). A case `r.out` holds a result of already is taken from there, so a
 * command started again goes on from what it saved: a run it continued or ran again is kept as it
 * is, and a stage still running gets its fresh runs. A case one of whose continued stages has an
 * arm whose copies come from its profile where one cannot be read (`savedArms`) is not written at
 * all, and says why.
 */
async function continueMixedCommand(
  r: Runner,
  from: string,
  logFile: string,
  onlyFile: string | undefined,
  furtherRounds: number | undefined
): Promise<void> {
  const saved: Behaviour = { ...r, out: from };
  const savedLogFile = path.join(from, REQUEST_LOG);
  let savedLog: SavedLog | undefined | null = null;
  const o: ContinueOptions = {
    from,
    continuations: loggedGateway(r.gateway, logFile, "continuation"),
    log: () => {
      if (savedLog === null) savedLog = readSavedLog(savedLogFile);
      return savedLog;
    },
    logFile: savedLogFile,
    only: onlyFile === undefined ? undefined : readOnly(onlyFile),
    furtherRounds,
  };
  /** Case `c`'s result to continue, from `r.out` where it holds one, else from `from`. */
  const input = (c: Case): { result: CaseResult; out: string } | undefined => {
    const own = readResult(r, c.id);
    if (own !== undefined) return { result: own, out: r.out };
    const result = readResult(saved, c.id);
    return result === undefined ? undefined : { result, out: from };
  };
  const held = new Set<string>();
  const checked = r.cases.flatMap((c) => {
    const each = input(c);
    if (each === undefined) return [];
    const places = stagePlaces(each.result);
    for (const place of places) held.add(`${c.id}\t${placeWords(place)}`);
    if (caseDrop(c, each.result) !== undefined) return [];
    return places.flatMap((place) => {
      const state = place.get(each.result);
      return state === undefined
        ? []
        : [{ caseId: c.id, label: placeWords(place), state, out: each.out }];
    });
  });
  const unheld = [...(o.only ?? [])].filter((each) => !held.has(each));
  if (unheld.length > 0) {
    throw new Refused(
      `--only ${onlyFile} names stages no case result of ${r.casesFile} under ${from} holds: ${unheld.map((each) => each.replaceAll("\t", " ")).join(", ")}`
    );
  }
  for (const out of new Set(checked.map((each) => each.out))) {
    refuseOtherRunners(
      r,
      out,
      checked.filter((each) => each.out === out),
      "behaviour continue-mixed continues a saved stage, or goes on with one it wrote,",
      "to continue those stages, or use another --from"
    );
  }
  say(
    r,
    `${r.cases.length} cases of the skill ${r.cases[0]?.skill} in ${r.casesFile}; the saved runs in ${from} the mixed-reply rule continues are continued into ${r.out}, ${o.only === undefined ? "every stage selected" : `the ${counted(o.only.size, "stage")} --only ${onlyFile} names selected`}, ${furtherRounds === undefined ? "each continuation as far as the live rule goes" : `each continuation at most ${counted(furtherRounds, "further round")}`}; texts ${textWords(r.texts)}`
  );
  for (const c of r.cases) {
    const phases = newPhases();
    const each = input(c);
    if (each === undefined) {
      say(r, `case ${c.id}: no saved result in ${from}, skipped`);
      continue;
    }
    let result = each.result;
    const drop = caseDrop(c, result);
    if (drop !== undefined) {
      const file = writeResult(r, result, phases);
      say(r, `case ${c.id}: dropped at ${drop.stage}, written as saved to ${file}: ${drop.reason}`);
      continue;
    }
    // The arms of every stage it continues, read before any of the case is written.
    const places = stagePlaces(result);
    const armsOf = new Map<StagePlace, ReadonlyMap<ArmName, SavedArm>>();
    const scratch = path.join(
      r.scratch,
      "arms",
      createHash("sha256").update(c.id).digest("hex").slice(0, 16)
    );
    let unreadable: string | undefined;
    for (const place of places) {
      const state = place.get(result);
      if (state === undefined || state.runs.length === 0) continue;
      if (!sameTexts(r.texts, state) || !selectedBy(o, c, place)) continue;
      const armed = await savedArms(r, c, place.stage, state, from, scratch);
      if (!(armed instanceof Map)) {
        unreadable = `its ${placeWords(place)}: ${armed.unreadable}`;
        break;
      }
      armsOf.set(place, armed);
    }
    if (unreadable !== undefined) {
      say(r, `case ${c.id}: not continued, and not written: ${unreadable}`);
      continue;
    }
    for (const place of places) {
      result = await continueStage(r, c, place, result, phases, o, armsOf.get(place));
      // A stage that drops the case leaves the stages after it as saved, as a stage command does.
      if (result.dropped !== null) break;
    }
    const last = places.at(-1)?.stage ?? "screen";
    await finishCase(r, result, last, phases);
  }
}

/**
 * `behaviour continue-mixed --probe <case id>/<arm>/<run number>`: rebuilds the continuation of
 * that one saved run under `from` as `continueStage` would (the run of that arm and number in the
 * case's stages the rule continues, `planRun`), and sends only its next request: one call, through
 * the request log as kind `probe`, with no later round and no judgement. It refuses (Refused, exit
 * 2), before any call, a value that is not of that form, a case the file or `from` lacks, and a
 * run the rule does not continue or whose record lacks a part, saying which; where two stages of
 * the case hold a run of that arm and number the rule continues, it refuses naming them. It
 * prints, and writes to `<out>/probe.json`, the probe: the run, both runners, its session and
 * routing ids, the deployment that answered its saved reply and where that came from, the one
 * that answered the probe and whether they are the same, the request's ids, its HTTP status (`ok`
 * for an answer, which the gateway gives only for a 2xx status and keeps no other figure of; a
 * refusal's status; null for a failure with none) or its error, the usage and the dollars at list
 * price. Gives 0 for an answer from the saved reply's deployment, 1 for any other outcome.
 */
async function probeCommand(
  r: Runner,
  from: string,
  probe: string,
  logFile: string
): Promise<number> {
  const at = probe.lastIndexOf("/");
  const armAt = probe.lastIndexOf("/", at - 1);
  const caseId = probe.slice(0, Math.max(0, armAt));
  const armName = probe.slice(armAt + 1, at);
  const number = Number(probe.slice(at + 1));
  const arms: readonly string[] = Stage.options.flatMap((stage) => stageArms(stage, r.compared));
  if (armAt <= 0 || !arms.includes(armName) || !Number.isInteger(number) || number < 1) {
    throw new Refused(
      `--probe ${probe} is not <case id>/<arm>/<run number>, an arm of ${[...new Set(arms)].join(", ")} and a run number from 1`
    );
  }
  const c = r.cases.find((each) => each.id === caseId);
  if (c === undefined)
    throw new Refused(`--probe ${probe}: ${r.casesFile} holds no case ${caseId}`);
  const result = readResult({ ...r, out: from }, caseId);
  if (result === undefined) {
    throw new Refused(`--probe ${probe}: ${from} holds no result of case ${caseId}`);
  }
  const savedLogFile = path.join(from, REQUEST_LOG);
  const log = (() => {
    let read: SavedLog | undefined | null = null;
    return () => {
      if (read === null) read = readSavedLog(savedLogFile);
      return read;
    };
  })();
  const scratch = path.join(r.scratch, "probe", createHash("sha256").update(c.id).digest("hex"));
  const rule: MixedRule = { kind: "case", act: caseAct(c) };
  const found: {
    place: StagePlace;
    state: StageResult;
    run: Run;
    saved: SavedArm;
    plan: Extract<RunPlan, { kind: "continue" }>;
  }[] = [];
  const notes: string[] = [];
  for (const place of stagePlaces(result)) {
    const state = place.get(result);
    const run = state?.runs.find((each) => each.arm === armName && each.run === number);
    if (state === undefined || run === undefined) continue;
    const words = placeWords(place);
    if (!sameTexts(r.texts, state)) {
      notes.push(
        `${words}: run with the texts ${textWords(state.texts)}, not ${textWords(r.texts)}`
      );
      continue;
    }
    const armed = await savedArms(r, c, place.stage, state, from, scratch);
    if (!(armed instanceof Map)) {
      notes.push(`${words}: ${armed.unreadable}`);
      continue;
    }
    const saved = armed.get(run.arm) as SavedArm;
    const sorted = sortRun(
      run,
      saved.arm,
      saved.copies,
      rule,
      caseKind(c) === "written",
      `case ${c.id} ${words} ${run.arm}`
    );
    const plan = planRun(run, sorted, saved, () => savedDeployment(run, log, savedLogFile));
    if (plan.kind === "continue") found.push({ place, state, run, saved, plan });
    else notes.push(`${words}: ${plan.kind === "keep" ? plan.why : plan.reason}`);
  }
  const [chosen, ...more] = found;
  if (chosen === undefined) {
    throw new Refused(
      `--probe ${probe}: the rule continues no saved run ${number} of case ${caseId}'s ${armName} arm${notes.length === 0 ? `: no stage of it holds one in ${from}` : `: ${notes.join("; ")}`}`
    );
  }
  if (more.length > 0) {
    throw new Refused(
      `--probe ${probe}: ${found.map((each) => placeWords(each.place)).join(" and ")} of case ${caseId} each hold a run ${number} of the ${armName} arm the rule continues, so the probe would not name one`
    );
  }
  const { place, state, run, saved, plan } = chosen;
  refuseOtherRunners(
    r,
    from,
    [{ caseId, label: placeWords(place), state }],
    "behaviour continue-mixed --probe continues a saved stage's run",
    "to probe it, or use another --from"
  );
  const capture = saved.capture as Capture;
  const { checkout } = await continuationCheckout(r, c);
  const gateway = loggedGateway(r.gateway, logFile, "probe");
  let reply: Reply | undefined;
  let failure: unknown;
  const watched: Gateway = {
    async send(body, options) {
      try {
        reply = await gateway.send(body, options);
        return reply;
      } catch (error) {
        failure = error;
        throw error;
      }
    },
    spend: () => gateway.spend(),
  };
  try {
    await resumeReplay(capture.body, savedReplyOf(run), saved.arm, watched, {
      headers: capture.record.requestHeaders,
      copies: saved.copies,
      mixed: rule,
      ...(checkout === undefined ? {} : { checkout }),
      endsAt: () => true,
    });
  } catch (error) {
    if (error instanceof NotResumable && reply === undefined) {
      throw new Refused(`--probe ${probe}: cannot rebuild the saved reply: ${error.message}`);
    }
    if (failure === undefined) throw error;
  }
  const answered = reply === undefined ? null : deploymentOf(reply);
  const same = answered === plan.deployment.id;
  const messageId: unknown = reply?.message.id;
  const written = {
    probe,
    caseId,
    stage: place.stage,
    ...(place.confirm === undefined ? {} : { confirm: place.confirm }),
    arm: run.arm,
    run: run.run,
    id: run.id,
    runners: { saved: savedRunNote(run, state).runner, probe: r.runner },
    userId: run.userId,
    routingId: run.routingId ?? null,
    deployment: { saved: plan.deployment, answered, same },
    request: {
      requestId: reply?.requestId ?? null,
      providerRequestId: reply?.providerRequestId ?? null,
      messageId: typeof messageId === "string" ? messageId : null,
      status:
        reply !== undefined
          ? "ok"
          : failure instanceof MessagesApiError
            ? (failure.status ?? null)
            : null,
      error: failure === undefined ? null : messageFor(failure),
    },
    usage: reply?.spent ?? null,
    usd: reply?.spent.usd ?? 0,
  };
  const file = path.join(r.out, PROBE_FILE);
  writeWhole(file, json(written));
  const outcome =
    reply === undefined
      ? `failed (${written.request.status === null ? "no status" : `status ${written.request.status}`}): ${written.request.error}`
      : `answered by deployment ${answered || "none named"}, ${same ? "the one" : `not ${plan.deployment.id}, the one`} that answered its saved reply (from ${plan.deployment.from === "run" ? "the run's record" : `the request log ${plan.deployment.file}`}); request id ${written.request.providerRequestId ?? "none"}; ${dollars(written.usd)} at list price`;
  say(r, `probe ${probe} (${placeWords(place)}): one request, ${outcome}; written to ${file}`);
  return reply !== undefined && same ? EXIT.ok : EXIT.failed;
}

// `behaviour`: the command.

const BEHAVIOUR_COMMANDS = [
  "screen",
  "confirm",
  "control",
  "report",
  "audit-mixed",
  "continue-mixed",
] as const;

const BEHAVIOUR_USAGE = [
  "usage: cli.ts behaviour <screen|confirm|control|report|audit-mixed|continue-mixed> --config <skill-cuts.json> --key-file <file> --out <dir> --old <dir> [--new <dir> | --ours <dir>] --cases <file>",
  "  [--planted-line <text>] [--budget-usd <n>] [--concurrency <n>] [--after-case <command>] [--request-log <file>]",
  "  [--models <models.yml>] [--omp <binary>] [--repos <dir>] [--hint-model <model>] [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>]",
  "  [--reading-models <file>] (the skill's <cut or set id>/models/<skill>.json, written by attribute)",
  `  confirm [--case <id>] (${FRESH_RUNS} fresh runs of old and the compared arm on that case alone, into its own --out)`,
  "  [--again <case id>]... (confirm only, not with --case: one more confirm of that flagged case)",
  "  [--confirm-flagged] (confirm only: confirm each flagged case even where the skill can get no verdict; report then gives case results only)",
  "  [--allow-runners <digest>[,<digest>...]] (take saved stages of these runners as this runner's; report: report results of these runners together)",
  "  [--store-only <file>] (report only: the skill's <cut id>/store-only/<skill>.json, written by attribute)",
  "  [--route-id <arm>=<uuid>]... (screen, confirm, control: a diagnostic: every run of that arm, in every round, goes with that routing id in place of its round's)",
  "  report needs only --config, --out, --old and --cases; its --request-log is the log it sets the spend against, <out>/requests.jsonl by default; [--limits <file>] (printed as it is)",
  "  [--build-summary <file>]... (report only: build-cases' summaries; it names their navigation lines no case tests and counts their replacement cases before and after the judge controls)",
  `  audit-mixed needs only --config, --out, --old and --cases, and --new or --ours as its stages were run with (neither for no skill), and reads --cache-dir and --scratch; it makes no model call and writes <out>/${AUDIT_FILE}`,
  "  continue-mixed --from <saved out> (required) continues the saved runs the mixed-reply rule continues, into a new --out. Give --new or --ours as the saved stages were run with (neither for no skill).",
  "    [--only <file>] (each line <case id>, a tab, and screen, control or confirm <n>: only those stages are continued; the others are kept as saved)",
  "    [--further-rounds <n>] (a continuation sends at most n requests; a reply it would continue again then waits with no verdict, further-round-limit)",
  `    [--probe <case id>/<arm>/<run number>] (send only that run's next request, with no judge, and write <out>/${PROBE_FILE}; not with --only or --further-rounds)`,
].join("\n");

/** The routing id each arm `--route-id <arm>=<uuid>` names is pinned to in `behaviour
 *  <command>`, from each value given: a diagnostic, in which every run of that arm, in every
 *  round, goes with that id in place of its round's (runStage). Refuses a value that is not
 *  `<arm>=<id>`, an arm the command does not run (score.ts's `stageArms` of the command's stage
 *  and the compared arm `compared`), an id that is not a UUID (the gateway's ROUTING_HEADER takes
 *  a UUID), an arm pinned twice, and any pin given to `report`, which makes no model call. */
function routePinsOf(
  given: readonly string[],
  command: string,
  compared: Compared
): Map<ArmName, string> {
  const pins = new Map<ArmName, string>();
  if (given.length === 0) return pins;
  const stage = Stage.safeParse(command);
  if (!stage.success) {
    throw new Refused(
      `--route-id goes with behaviour screen, confirm and control only: ${command} makes no model call. ${BEHAVIOUR_USAGE}`
    );
  }
  const arms: readonly ArmName[] = stageArms(stage.data, compared);
  for (const pin of given) {
    const at = pin.indexOf("=");
    if (at < 0) throw new Refused(`--route-id ${pin} is not <arm>=<uuid>. ${BEHAVIOUR_USAGE}`);
    const arm = arms.find((each) => each === pin.slice(0, at));
    const id = pin.slice(at + 1);
    if (arm === undefined) {
      throw new Refused(
        `--route-id ${pin}: behaviour ${command} runs no arm ${JSON.stringify(pin.slice(0, at))}; its arms are ${arms.join(", ")}`
      );
    }
    if (!isUuid(id)) {
      throw new Refused(
        `--route-id ${pin}: the routing id is not a UUID, and the gateway's ${ROUTING_HEADER} takes a UUID`
      );
    }
    if (pins.has(arm)) throw new Refused(`--route-id pins the ${arm} arm twice`);
    pins.set(arm, id);
  }
  return pins;
}

/** The compared arm the stages of `results` record (a stage that records none was run with
 *  `--new`); undefined when no stage was run. Throws a refusal naming two files under `out` that
 *  record different arms. */
function recordedCompared(
  out: string,
  results: ReadonlyMap<string, CaseResult>
): Compared | undefined {
  let found: { compared: Compared; file: string } | undefined;
  for (const result of results.values()) {
    for (const stage of Stage.options) {
      const state = result.stages[stage];
      if (state === undefined) continue;
      const each = { compared: recordedArm(state), file: caseResultFile(out, result.caseId) };
      found ??= each;
      if (found.compared !== each.compared) {
        throw new Refused(
          `${found.file} holds runs made with the compared arm ${found.compared}, and ${each.file} with ${each.compared}: one --out holds the runs of one compared arm`
        );
      }
    }
  }
  return found?.compared;
}

/** `skill-cuts behaviour <command>`. */
async function behaviourCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const [command, ...rest] = argv;
  if (!(BEHAVIOUR_COMMANDS as readonly (string | undefined)[]).includes(command)) {
    throw new Refused(`behaviour ${command ?? ""}: no such command. ${BEHAVIOUR_USAGE}`);
  }
  const values = parse(
    rest,
    {
      ...GATEWAY_OPTIONS,
      out: { type: "string" },
      old: { type: "string" },
      new: { type: "string" },
      ours: { type: "string" },
      cases: { type: "string" },
      case: { type: "string" },
      "planted-line": { type: "string" },
      "after-case": { type: "string" },
      models: { type: "string" },
      omp: { type: "string" },
      repos: { type: "string" },
      "hint-model": { type: "string" },
      "omp-config": { type: "string", multiple: true },
      "cache-dir": { type: "string" },
      scratch: { type: "string" },
      "reading-models": { type: "string" },
      limits: { type: "string" },
      "build-summary": { type: "string", multiple: true },
      again: { type: "string", multiple: true },
      "confirm-flagged": { type: "boolean" },
      "allow-runners": { type: "string" },
      "store-only": { type: "string" },
      "route-id": { type: "string", multiple: true },
      from: { type: "string" },
      probe: { type: "string" },
      only: { type: "string" },
      "further-rounds": { type: "string" },
    },
    BEHAVIOUR_USAGE
  );
  if (values.again !== undefined && command !== "confirm") {
    throw new Refused(`--again goes with behaviour confirm only. ${BEHAVIOUR_USAGE}`);
  }
  if (values["confirm-flagged"] !== undefined && command !== "confirm") {
    throw new Refused(
      `--confirm-flagged goes with behaviour confirm only: report reads from the case results whether their confirms were made with it. ${BEHAVIOUR_USAGE}`
    );
  }
  const storeOnlyFile = values["store-only"];
  if (storeOnlyFile !== undefined && command !== "report") {
    throw new Refused(
      `--store-only goes with behaviour report only: no stage command reads it. ${BEHAVIOUR_USAGE}`
    );
  }
  const continuing = command === "continue-mixed";
  for (const name of ["from", "probe", "only", "further-rounds"] as const) {
    if (values[name] !== undefined && !continuing) {
      throw new Refused(`--${name} goes with behaviour continue-mixed only. ${BEHAVIOUR_USAGE}`);
    }
  }
  if (values.probe !== undefined && (values.only ?? values["further-rounds"]) !== undefined) {
    throw new Refused(
      `--probe sends one run's next request alone, so it takes no --only and no --further-rounds. ${BEHAVIOUR_USAGE}`
    );
  }
  const furtherGiven = values["further-rounds"];
  const furtherRounds = furtherGiven === undefined ? undefined : Number(furtherGiven);
  if (furtherRounds !== undefined && (!Number.isInteger(furtherRounds) || furtherRounds < 1)) {
    throw new Refused(`--further-rounds ${furtherGiven} is not a whole number of 1 or more`);
  }
  const allowRunners = values["allow-runners"]?.split(",").map((each) => each.trim()) ?? [];
  if (allowRunners.includes("")) {
    throw new Refused(
      `--allow-runners ${JSON.stringify(values["allow-runners"])} names an empty runner: give each runner's digest, joined by commas. ${BEHAVIOUR_USAGE}`
    );
  }
  const report = command === "report";
  const audit = command === "audit-mixed";
  const given = required(
    values,
    report || audit
      ? ["config", "out", "old", "cases"]
      : continuing
        ? ["config", "key-file", "out", "from", "old", "cases"]
        : ["config", "key-file", "out", "old", "cases"],
    BEHAVIOUR_USAGE
  );
  if (values.new !== undefined && values.ours !== undefined) {
    throw new Refused(
      `--new and --ours exclude each other: give one, or neither to compare the old text with no skill. ${BEHAVIOUR_USAGE}`
    );
  }
  if (values.case !== undefined && command !== "confirm") {
    throw new Refused("--case is for behaviour confirm only");
  }
  if (values.case !== undefined && values.again !== undefined) {
    throw new Refused(
      "--again and --case exclude each other: --case runs one confirm of its case alone, into its own --out"
    );
  }
  if (values.limits !== undefined && !report) {
    throw new Refused("--limits is for behaviour report only");
  }
  if (values["build-summary"] !== undefined && !report) {
    throw new Refused("--build-summary is for behaviour report only");
  }
  const named: Compared =
    values.new !== undefined ? "new" : values.ours !== undefined ? "ours" : "none";
  const routePins = routePinsOf(values["route-id"] ?? [], command ?? "", named);
  // A stage command writes the cases' session text under --out; `report` writes nothing, and
  // `audit-mixed` only ids, counts and tool names.
  const inCheckout = report || audit ? undefined : outRefusal(given.out);
  if (inCheckout !== undefined) throw new Refused(inCheckout);
  const config = configFrom(given.config);
  let cases: Case[];
  try {
    cases = loadCases(given.cases, given.old);
  } catch (error) {
    throw new Refused(messageFor(error));
  }
  if (cases.length === 0) throw new Refused(`${given.cases} holds no case`);
  const one = values.case === undefined ? undefined : cases.find((c) => c.id === values.case);
  if (values.case !== undefined && one === undefined) {
    throw new Refused(`--case ${values.case}: ${given.cases} holds no case ${values.case}`);
  }
  if (one !== undefined && servedModel(one.model, config.servedModels) === undefined) {
    throw new Refused(`--case ${one.id}: ${notServedWords(one.model, config.servedModels)}`);
  }
  const readingModels = values["reading-models"];
  const limits = values.limits;
  const behaviour: Behaviour = {
    io,
    command: `behaviour ${command}`,
    out: given.out,
    casesFile: given.cases,
    cases,
    compared: named,
    checkCompared:
      !audit && !continuing && (!report || values.new !== undefined || values.ours !== undefined),
    servedModels: config.servedModels,
    readingModels:
      readingModels === undefined
        ? undefined
        : {
            file: readingModels,
            count: modelCountOf(
              readJson(readingModels, "the reading sessions by model"),
              readingModels,
              "--reading-models"
            ),
          },
    limits:
      limits === undefined
        ? undefined
        : { file: limits, text: readBytes(limits, "the limits text").toString("utf8") },
    allowRunners,
    storeOnly: storeOnlyFile === undefined ? undefined : readStoreOnly(storeOnlyFile),
  };
  const log = values["request-log"] ?? path.join(given.out, REQUEST_LOG);
  if (report) {
    const summaries = values["build-summary"];
    reportCommand(
      behaviour,
      given.old,
      summaries === undefined ? undefined : buildNavigation(summaries),
      log
    );
    return EXIT.ok;
  }
  // The served-sessions refusal stops the session cases only: a skill with written cases is
  // screened on them (screenCommand).
  const refusal = skillRefusal(behaviour);
  if (command === "screen" && refusal !== undefined) {
    throw new Refused(
      `the skill ${cases[0]?.skill} gets no verdict, and no case of it is screened: ${refusal.reason} (--reading-models ${readingModels}); the walk of the cut's ranking goes on to its next skill`
    );
  }

  const comparedDir = values.new ?? values.ours;
  if (
    comparedDir !== undefined &&
    !statSync(comparedDir, { throwIfNoEntry: false })?.isDirectory()
  ) {
    throw new Refused(`--${named} ${comparedDir} is not a directory`);
  }
  if (values.ours !== undefined && !isFile(path.join(values.ours, "SKILL.md"))) {
    throw new Refused(`--ours ${values.ours} holds no SKILL.md`);
  }
  const scratch = values.scratch ?? path.join(tmpdir(), SCRATCH_DIR);
  const cacheDir = values["cache-dir"] ?? path.join(tmpdir(), CACHE_DIR);
  const texts: Texts = {
    old: textDigest(given.old),
    ...(values.new === undefined ? {} : { new: textDigest(values.new) }),
    ...(values.ours === undefined ? {} : { ours: textDigest(values.ours) }),
  };
  // The arms' directories but the planted line, which a stage command takes from --planted-line
  // and `audit-mixed` from each control stage it reads.
  const dirs: ArmDirs = {
    old: given.old,
    ...(values.new === undefined ? {} : { new: values.new }),
    ...(values.ours === undefined
      ? {}
      : { ours: { dir: values.ours, name: skillName(values.ours) } }),
  };
  if (audit) {
    auditMixedCommand(behaviour, dirs, texts, cacheDir, scratch);
    return EXIT.ok;
  }
  const from = values.from;
  if (continuing) {
    if (from === undefined || !statSync(from, { throwIfNoEntry: false })?.isDirectory()) {
      throw new Refused(
        `--from ${from ?? ""} is not a directory: give the saved --out to continue`
      );
    }
    if (path.resolve(from) === path.resolve(given.out)) {
      throw new Refused(
        `--out ${given.out} is the --from it continues, and continue-mixed never writes the saved --out: give a new --out`
      );
    }
  }
  const unpriced = [
    ...new Set(
      cases.flatMap((c) => {
        const served = servedModel(c.model, config.servedModels);
        return served !== undefined && PRICES[modelId(served)] === undefined
          ? [modelId(served)]
          : [];
      })
    ),
  ];
  if (unpriced.length > 0) {
    throw new Refused(
      `gateway.ts has no list price for the cases' models ${unpriced.join(", ")}, so their dollars could not be counted: add each to PRICES from ${PRICE_SOURCE}`
    );
  }
  // Only a command given --models can make a capture, so only it needs Oh My Pi; without it every
  // capture is read back, and Oh My Pi does not run.
  const models = values.models;
  const omp = models === undefined ? undefined : requiredOmp(values.omp);
  const gateway = openGateway({
    config,
    keyFile: given["key-file"],
    budgetUsd: values["budget-usd"],
    concurrency: values.concurrency,
    spendRecorder: io.spendRecorder,
  });
  const plantedLine = values["planted-line"];
  const source = values.repos === undefined ? ghSource() : localSource(values.repos);
  const hints = gatewayHints(loggedGateway(gateway, log, "hint"));
  const runner: Runner = {
    ...behaviour,
    dirs: { ...dirs, ...(plantedLine === undefined ? {} : { plantedLine }) },
    texts,
    scratch,
    cacheDir,
    gateway,
    replays: loggedGateway(gateway, log, "replay"),
    judge: loggedGateway(gateway, log, "judge"),
    source,
    gitRemote: (repo) =>
      values.repos === undefined ? githubRemote(repo) : path.join(values.repos, `${repo}.git`),
    dotfilesRepo: config.dotfilesRepo,
    capture: async (c, arm) => {
      const outDir = armCaptureDir(given.out, arm, texts);
      const saved = await restoredCapture(c, arm, capturePaths(outDir, c.id, arm.name), {
        source,
        cacheDir,
      });
      if (saved !== undefined) return saved;
      if (models === undefined || omp === undefined) {
        throw new Error(
          `case ${c.id} has no capture of its ${arm.name} arm in ${outDir}, and no --models is given to make one`
        );
      }
      // A case runs on the served model that serves it: an Anthropic model behind another
      // gateway prefix as the served `anthropic/<id>`, so the capture finds its provider.
      const model = servedModel(c.model, config.servedModels) ?? c.model;
      return captureBody({ ...c, model }, arm, {
        omp,
        outDir,
        scratchDir: path.join(scratch, "captures"),
        cacheDir,
        source,
        models,
        hints,
        ...(values["hint-model"] === undefined ? {} : { hintModel: values["hint-model"] }),
        configFiles: values["omp-config"] ?? [],
        session: await caseSession(config, given.out, c),
      });
    },
    afterCase: values["after-case"],
    afterCaseFailures: [],
    runner: runnerDigest(),
    again: values.again ?? [],
    confirmFlagged: values["confirm-flagged"] === true,
    routePins,
  };
  if (routePins.size > 0) {
    const pins = [...routePins].map(([arm, id]) => `${arm}=${id}`).join(", ");
    say(
      runner,
      `routing pinned: ${pins}: every run of a pinned arm goes with that routing id in every round, in place of its round's, so these runs are a diagnostic, not a test result`
    );
  }
  if (continuing && from !== undefined) {
    if (values.probe !== undefined) return await probeCommand(runner, from, values.probe, log);
    await continueMixedCommand(runner, from, log, values.only, furtherRounds);
  } else if (command === "screen") await screenCommand(runner);
  else if (command === "confirm" && one !== undefined) await confirmCaseCommand(runner, one);
  else if (command === "confirm") await confirmCommand(runner);
  else await controlStageCommand(runner);
  say(
    runner,
    `done; the calls of this run cost $${gateway.spend().usd.toFixed(4)} at list price; request log ${log}`
  );
  if (runner.afterCaseFailures.length > 0) {
    io.err(
      `${runner.command}: the --after-case command failed for ${runner.afterCaseFailures.join(", ")}`
    );
    return EXIT.failed;
  }
  return EXIT.ok;
}

// `load`.

const LOAD_USAGE = [
  "usage: cli.ts load --config <skill-cuts.json> --key-file <file> --skill <dir> [--name <name>] --tasks <file> --host <omp|claude> [--profile <file>] --out <dir>",
  "  --profile is the profile of each task whose line names no profile of its own",
  `  [--runs <n, default ${LOAD_RUNS}>] [--model <model, default ${LOAD_MODEL}>] [--thinking <level, default ${LOAD_THINKING.omp} on omp, ${LOAD_THINKING.claude} on claude>]`,
  "  [--budget-usd <n>] [--concurrency <n>] [--request-log <file>] [--repos <dir>] [--cache-dir <dir>] [--scratch <dir>]",
  "  [--context-session <store>:<path hash>] (the session a written task stands for: the one its profile was rebuilt from)",
  `  [--test-setup <owner/repo>@<commit>:<path>] (add the inventory copy; ${TEST_SETUP_LABEL})`,
  "  [--route-seed <text>] (empty by default: each run's routing id is derived from it, the task and the run number, so two loads of one seed send the same id for run n of a task)",
  "  omp: [--models <models.yml>] [--omp <binary>] [--hint-model <model>] [--hint-draw <n, default 1>] [--omp-config <file>]...",
  "  claude: [--claude <binary>]",
].join("\n");

/** The whole number of 1 or more `--<name>` gives as `value`, or `fallback` when it is not given.
 *  Throws a refusal naming the option on any other value. */
function wholeOption(name: string, value: string | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new Refused(`--${name} ${value} is not a whole number of 1 or more`);
  }
  return Number(value);
}

/** `skill-cuts load`: the load test of load.ts. */
async function loadCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const values = parse(
    argv,
    {
      ...GATEWAY_OPTIONS,
      skill: { type: "string" },
      name: { type: "string" },
      tasks: { type: "string" },
      host: { type: "string" },
      profile: { type: "string" },
      out: { type: "string" },
      runs: { type: "string" },
      model: { type: "string" },
      thinking: { type: "string" },
      models: { type: "string" },
      omp: { type: "string" },
      "hint-model": { type: "string" },
      "hint-draw": { type: "string" },
      "omp-config": { type: "string", multiple: true },
      claude: { type: "string" },
      repos: { type: "string" },
      "cache-dir": { type: "string" },
      scratch: { type: "string" },
      "context-session": { type: "string" },
      "test-setup": { type: "string" },
      "route-seed": { type: "string" },
    },
    LOAD_USAGE
  );
  const given = required(
    values,
    ["config", "key-file", "skill", "tasks", "host", "out"],
    LOAD_USAGE
  );
  // The load test writes the tasks' session text under --out.
  const inCheckout = outRefusal(given.out);
  if (inCheckout !== undefined) throw new Refused(inCheckout);
  const host = Host.safeParse(given.host);
  if (!host.success) throw new Refused(`--host ${given.host} is no host: omp or claude`);
  const thinking = Thinking.safeParse(values.thinking ?? LOAD_THINKING[host.data]);
  if (!thinking.success) {
    throw new Refused(
      `--thinking ${values.thinking} is no thinking level: ${Thinking.options.join(", ")}`
    );
  }
  const omp =
    host.data === "omp" && values.models !== undefined ? requiredOmp(values.omp) : undefined;
  const runs = wholeOption("runs", values.runs, LOAD_RUNS);
  const hintDraw = wholeOption("hint-draw", values["hint-draw"], 1);
  if (!statSync(given.skill, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Refused(`--skill ${given.skill} is not a directory`);
  }
  const config = configFrom(given.config);
  const named = values["context-session"];
  let contextSession: { store: string; pathHash: string } | undefined;
  if (named !== undefined) {
    const match = /^([^:]+):([0-9a-f]{64})$/.exec(named);
    if (match?.[1] === undefined || match[2] === undefined) {
      throw new Refused(
        `--context-session ${named} is not <store>:<path hash>, the hash the SHA-256 of the session file's path in hex`
      );
    }
    if (!config.stores.some((store) => store.name === match[1])) {
      throw new Refused(`--context-session ${named}: the configuration names no store ${match[1]}`);
    }
    contextSession = { store: match[1], pathHash: match[2] };
  }
  const testSetup =
    values["test-setup"] === undefined ? undefined : testSetupOption(values["test-setup"]);
  const gateway = openGateway({
    config,
    keyFile: given["key-file"],
    budgetUsd: values["budget-usd"],
    concurrency: values.concurrency,
    spendRecorder: io.spendRecorder,
  });
  const log = values["request-log"] ?? path.join(given.out, REQUEST_LOG);
  const result = await runLoad({
    io,
    host: host.data,
    skillDir: given.skill,
    skillDigest: textDigest(given.skill),
    ...(values.name === undefined ? {} : { name: values.name }),
    tasksFile: given.tasks,
    ...(values.profile === undefined ? {} : { profileFile: values.profile }),
    runs,
    out: given.out,
    model: values.model ?? LOAD_MODEL,
    thinking: thinking.data,
    gateway: loggedGateway(gateway, log, "load"),
    hints: gatewayHints(loggedGateway(gateway, log, "hint")),
    source: values.repos === undefined ? ghSource() : localSource(values.repos),
    scratch: values.scratch ?? path.join(tmpdir(), SCRATCH_DIR),
    cacheDir: values["cache-dir"] ?? path.join(tmpdir(), CACHE_DIR),
    ...(omp === undefined ? {} : { omp }),
    ...(values.models === undefined ? {} : { models: values.models }),
    ...(values["hint-model"] === undefined ? {} : { hintModel: values["hint-model"] }),
    hintDraw,
    configFiles: values["omp-config"] ?? [],
    claude: values.claude ?? "claude",
    requestLog: log,
    memoryLimitBytes: RUN_MEMORY_LIMIT_BYTES,
    psqlCommand: config.psqlCommand,
    stores: config.stores.map((store) => store.name),
    ...(config.devboxClaudeConfigDir === undefined
      ? {}
      : { devboxClaudeConfigDir: config.devboxClaudeConfigDir }),
    ...(contextSession === undefined ? {} : { contextSession }),
    ...(testSetup === undefined ? {} : { testSetup }),
    routeSeed: values["route-seed"] ?? "",
  });
  io.out(
    `load: done; the calls of this command cost $${gateway.spend().usd.toFixed(4)} at list price; request log ${log}\n`
  );
  if (result.failures.length === 0) return EXIT.ok;
  io.err(
    `load: ${result.failures.length} tasks or runs failed; run the command again to make what is missing`
  );
  return EXIT.failed;
}

const LOAD_AUDIT_USAGE = `usage: cli.ts load audit-mixed --out <dir> --skill <dir> [--cache-dir <dir>]: replay.ts's mixed-reply rule (LOAD_RULE) over the saved runs of a load test's --out, with no model call; it writes <out>/${AUDIT_FILE}`;

/** `skill-cuts load audit-mixed`: load.ts's `auditLoadMixed` over the saved runs under `--out`,
 *  the tested skill's directory `--skill` (as the load test was given it) answering a read of its
 *  files, and the installed copies the tasks' captures name under `--cache-dir` (the load test's
 *  default) answering a read of another skill. Prints, per task, its runs and judged runs, and
 *  the runs whose last reply read skill files beside other calls, by what the rule makes of it
 *  now, each named with its run number and the tools that decided it, and writes that as JSON to
 *  `<out>/audit-mixed.json`, which it names: ids, counts and tool names, never a reply's text. */
function loadAuditCommand(argv: readonly string[], io: CliIo): number {
  const values = parse(
    argv,
    { out: { type: "string" }, skill: { type: "string" }, "cache-dir": { type: "string" } },
    LOAD_AUDIT_USAGE
  );
  const given = required(values, ["out", "skill"], LOAD_AUDIT_USAGE);
  if (!statSync(given.skill, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Refused(`--skill ${given.skill} is not a directory`);
  }
  const tasks = auditLoadMixed({
    out: given.out,
    skillDir: path.resolve(given.skill),
    cacheDir: values["cache-dir"] ?? path.join(tmpdir(), CACHE_DIR),
  });
  const line = (text: string) => io.out(`load audit-mixed: ${text}\n`);
  for (const task of tasks) {
    line(
      `task ${task.task}: ${task.runs} runs, ${task.judged} judged; ended before the rule ${task.untouched}; ended at an action ${auditedWords(task.actions)}; continued now ${auditedWords(task.continues)}; sent by another skill's answer now ${auditedWords(task.sentBy)}; no verdict past the last round ${auditedWords(task.pastLastRound)}; unresolved ${auditedWords(task.unresolved)}; only skill reads ${task.skillRound}; no mixed reply ${task.ends}`
    );
  }
  const sum = (count: (task: TaskAudit) => number) =>
    tasks.reduce((total, task) => total + count(task), 0);
  const totals = {
    runs: sum((task) => task.runs),
    judged: sum((task) => task.judged),
    actions: sum((task) => task.actions.length),
    continues: sum((task) => task.continues.length),
    sentBy: sum((task) => task.sentBy.length),
    pastLastRound: sum((task) => task.pastLastRound.length),
    unresolved: sum((task) => task.unresolved.length),
  };
  const file = path.join(given.out, AUDIT_FILE);
  writeWhole(
    file,
    json({ out: given.out, skill: given.skill, maxSkillRounds: MAX_SKILL_ROUNDS, totals, tasks })
  );
  line(
    `${totals.runs} runs, ${totals.judged} judged: ${totals.actions} ended at an action, ${totals.continues} continued now, ${totals.sentBy} sent by another skill's answer now, ${totals.pastLastRound} with no verdict past the last round, ${totals.unresolved} unresolved; written to ${file}`
  );
  return EXIT.ok;
}

// The command line.

const USAGE = [
  "usage: cli.ts <command> ...",
  "  behaviour <screen|confirm|control|report|audit-mixed|continue-mixed> --config <skill-cuts.json> --key-file <file> --out <dir> --old <dir> [--new <dir> | --ours <dir>] --cases <file> ...",
  "  read --prompt <name> --in <input.json> --out <answer.json> ...",
  "  control <map|attack|same-job|sentence> --in <input.json> --out <result.json> ...",
  "  control craft (--remove-section <heading> | --join-lines <a>-<b>) --copy-line <a>[-<b>] ... --original-line <a>[-<b>|all] ... <craft's flags> --out <dir>",
  "  craft-lines --list <craft-list.json>",
  `  craft ${CRAFT_FLAGS} --out <entry.json> ...`,
  "  craft-merge --entry <entry.json> --entry <entry.json> [--entry <entry.json>]... --out <merged.json>",
  `  nearest ${NEAREST_FLAGS} ...`,
  "  craft-coverage --root <dir> [--source <text>] [--skip <name>]... --entries <reader-findings.jsonl>",
  `  proposals ${PROPOSALS_FLAGS} ...`,
  `  proposals ${PROPOSALS_DRY_RUN}`,
  "  count-sql --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>] [--query count|models]",
  "  count-sql --config <skill-cuts.json> --store <db> --query roots|hashes",
  "  count-sql --config <skill-cuts.json> --query transcript --day <YYYY-MM-DD> [--hour <0-23>] [--run]",
  "  count-sql --config <skill-cuts.json> --query transcript-coverage [--run]",
  "  load --config <skill-cuts.json> --key-file <file> --skill <dir> [--name <name>] --tasks <file> --host <omp|claude> [--profile <file>] [--test-setup <owner/repo>@<commit>:<path>] --out <dir> ...",
  "  load audit-mixed --out <dir> --skill <dir> [--cache-dir <dir>]",
  "  copies --config <skill-cuts.json> --at <ISO instant> --out <dir> [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]",
  "  attribute --config <skill-cuts.json> --count <dir> --copies <dir> --spans <file> --out <dir> [--changed <cut or set id>=<file>]... [--models <dir>]",
  "  set-count --config <skill-cuts.json> --rows <dir> --spans <file> --out <dir> [--dotfiles-commit <sha>] [--rows-label <text>] [--not-read <name>=<reason>]...",
  "  build-cases --config <skill-cuts.json> --key-file <file> (--cut <id> | --set <id>) --skill <name> --old <dir> [--new <dir>] --sessions <file> --seed <n> --copies <dir> --count <dir> --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>] [--drop-navigation] [--store-only <file>]",
  "  build-cases --config <skill-cuts.json> --key-file <file> (--cut <id> | --set <id>) --skill <name> --old <dir> --copies <dir> --count <dir> --written <n> (--context-session <store>:<hash> | --sessions <file> --seed <n>) --out <cases.jsonl> [--session-cases <cases.jsonl>] [--reading-models <file>] [--model-pool <file>]... [--written-answer <file>] [--budget-usd <n>] [--drop-navigation]",
  "  build-cases --config <skill-cuts.json> --old <dir> --copies <dir> --count <dir> --reprofile <cases.jsonl> --out <new cases.jsonl>",
  "  build-tasks --config <skill-cuts.json> --key-file <file> --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir> --context-session <store>:<hash> [--test-setup <owner/repo>@<commit>:<path>]] [--max-candidates <n>] [--max-search-minutes <m>]",
  "  build-tasks --config <skill-cuts.json> --copies <dir> --count <dir> --reprofile <tasks.jsonl> --profiles-out <new dir> [--context-session <store>:<hash>]",
  "  build-tasks --first-read --config <skill-cuts.json> --key-file <file> --skill <dir> --sessions <sessions.jsonl> --seed <n> [--max <n>] --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir>]",
  "  results-index --count <set-count out dir> --nearest <dir> --cases <dir> --results <dir>",
].join("\n");

/** The command line. Returns the exit status (EXIT), once the usage spans its model calls queued
 *  are sent (`io.spendRecorder`'s `flush`, at most FLUSH_MS), on every way out: the one wait of
 *  the process, `build-cases` and `build-tasks` among them, whose build.ts `main` leaves it to its
 *  caller. */
export async function main(argv: readonly string[], io: CliIo): Promise<number> {
  const [command, ...rest] = argv;
  try {
    switch (command) {
      case "behaviour":
        return await behaviourCommand(rest, io);
      case "read":
        return await readCommand(rest, io);
      case "control":
        return await controlCommand(rest, io);
      case "count-sql":
        return countSqlMain(rest, io);
      case "load":
        return rest[0] === "audit-mixed"
          ? loadAuditCommand(rest.slice(1), io)
          : await loadCommand(rest, io);
      case "copies":
        return await copiesMain(rest, {
          gh: spawnGh,
          sleep: (ms) => Bun.sleep(ms),
          now: () => Date.now(),
          out: io.out,
          err: io.err,
        });
      case "attribute":
        return attributeMain(rest, io);
      case "set-count":
        return await setCountMain(rest, { out: io.out, err: io.err, gh: spawnGh });
      case "build-cases":
      case "build-tasks":
        return await buildMain(argv, {
          out: io.out,
          err: io.err,
          gh: spawnGh,
          now: () => performance.now(),
          spendRecorder: io.spendRecorder,
        });
      case "craft-lines":
        return await craftLinesMain(rest, { out: io.out, err: io.err, gh: spawnGh });
      case "craft":
        return await craftCommand(rest, io);
      case "craft-merge":
        return craftMergeMain(rest, io);
      case "nearest":
        return await nearestCommand(rest, io);
      case "craft-coverage":
        return craftCoverageCommand(rest, io);
      case "proposals":
        return await proposalsCommand(rest, io);
      case "results-index":
        return resultsIndexMain(rest, io);
      default:
        throw new Refused(`no command ${JSON.stringify(command ?? "")}. ${USAGE}`);
    }
  } catch (error) {
    if (error instanceof Refused) {
      io.err(`skill-cuts: ${error.message}`);
      return EXIT.refused;
    }
    if (error instanceof Stopped || error instanceof BudgetSpent) {
      io.err(`skill-cuts: stopped: ${error.message}`);
      return EXIT.stopped;
    }
    io.err(`skill-cuts: ${command}: ${messageFor(error)}`);
    return EXIT.failed;
  } finally {
    await io.spendRecorder?.flush();
  }
}

if (import.meta.main) {
  process.exit(await main(Bun.argv.slice(2), processIo()));
}
