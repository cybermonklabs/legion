/**
 * The skill-cuts runner's command line (README.md, "Commands"). Every command that calls a model
 * takes `--config <skill-cuts.json>` (config.ts: the gateway's address), `--key-file <file>` (the
 * gateway key, read inside gateway.ts and never printed, logged or written), and `--out` (where
 * it writes; `behaviour`'s stage commands, `load` and `build-cases` write session text there, so
 * they refuse an `--out` inside a git or jj checkout, exit 2).
 * `behaviour` and `load` read skill texts from directories, cases or tasks from a file, and the
 * key from a file; the repositories a context needs come from `--repos <dir>` (a tree of
 * `<owner>/<name>/<commit>/` directories, context.ts's `localSource`), or else from GitHub
 * through `gh` (context.ts's `ghSource`). So they read GitHub, with its token, only to make a
 * capture with no `--repos`, to make again the installed copies a saved capture names where the
 * cache lacks them, and, for a load with `--test-setup`, to read its inventory copy.
 *
 *   bun scripts/skill-cuts/cli.ts behaviour <screen|confirm|control|report> --config <file>
 *       --key-file <file> --out <dir> --old <dir> --new <dir> --cases <file>
 *       [--planted-line <text>] [--budget-usd <n>] [--concurrency <n>] [--after-case <command>]
 *       [--models <models.yml>] [--omp <binary>] [--repos <dir>] [--hint-model <model>]
 *       [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>] [--request-log <file>]
 *       [--reading-models <file>] [--again <case id>]... [--confirm-flagged] (both confirm only)
 *       [--control-informative] (control only)
 *       [--allow-runners <digest>[,<digest>...]] [--store-only <file>] [--merged] (report only)
 *       [--route-id <arm>=<uuid>]... (screen, confirm and control only)
 *   bun scripts/skill-cuts/cli.ts behaviour audit-mixed --config <file> --out <dir> --old <dir>
 *       --new <dir> --cases <file> [--cache-dir <dir>] [--scratch <dir>]
 *   bun scripts/skill-cuts/cli.ts behaviour continue-mixed --config <file> --key-file <file>
 *       --from <saved out> --out <new dir> --old <dir> --new <dir> --cases <file>
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
 *       --spans <file> --out <dir> [--changed <cut id>=<file>]... [--models <dir>]
 *   bun scripts/skill-cuts/cli.ts build-cases --config <file> --key-file <file> --cut <id>
 *       --skill <name> --old <dir> [--new <dir>] --sessions <file> --seed <n> --copies <dir>
 *       --count <dir> --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>]
 *       [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]
 *       [--store-only <file>]
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
 *
 * `count-sql`, `copies`, `attribute`, `build-cases` and `build-tasks` are the commands of
 * countsql.ts, copies.ts, attribute.ts and build.ts, given their arguments whole.
 *
 * **`behaviour`** runs the behaviour test of one skill: `--old` and `--new` are the skill's own
 * directories at the cut's base and in the cut, and `--cases` a case file of that skill (cases.ts,
 * checked against `--old`). Each case's results are one file, `<out>/cases/<id>.json` (CaseResult),
 * written whole (a fresh file renamed over it) after each round of runs, so a run started again
 * goes on from what was saved and skips each case whose stage is complete. `--after-case
 * <command>` runs `<command> <that file>` through `sh` when a case's stage ends; one still
 * running after 10 minutes (AFTER_CASE_TIMEOUT_MS) is ended, and counts as failed.
 *
 * - `screen`: per case in the file's order, until the skill has MIN_INFORMATIVE_CASES informative
 *   or MAX_SCREENED_CASES screened cases (score.ts): a case whose model the gateway does not serve
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
 *   per arm (old, new, none; capture.ts, read back when saved already), SCREEN_RUNS replays of
 *   each, every reply of the round judged blind (all arms shuffled under a recorded seed), and
 *   rounds of extra runs for the runs with no verdict, as score.ts's `stageOutcome` says, at most
 *   MAX_EXTRA_RUNS per arm. With `--reading-models <file>` (attribute.ts's
 *   `<cut id>/models/<skill>.json` of the skill: its reading sessions by model), a skill whose
 *   reading sessions on a model the gateway serves are fewer than MIN_READING_SESSIONS is refused
 *   before any model call (score.ts's `servedScreeningRefusal`: `too few sessions on served
 *   models`, with the served and total counts): it gets no verdict, and the walk of the cut's
 *   ranking goes on to its next skill.
 * - `confirm`: FRESH_RUNS fresh runs of old and new for each flagged case that is not dropped. A
 *   confirm records the runner that made it (`runnerDigest`). Where a case's last two confirms at
 *   the same texts on one runner disagree, one a loss and one not, `confirm` runs a third, and
 *   score.ts pools the three (`confirmScore`). `--again <case id>` asks for one more confirm of a
 *   flagged case whose confirm is finished; it is refused (exit 2), naming the case, where the
 *   case already has POOLED_CONFIRMS confirms at these texts on this runner (no fourth runs), or
 *   two that agree (a third runs only where two disagree). `--confirm-flagged` confirms each
 *   flagged case that is not dropped whatever the skill's informative count or its other reasons
 *   for no verdict (`noVerdict`), so a case run again alone can be confirmed: the same confirm
 *   stage, third confirm and `--again` as without it. It never confirms a case its screen did not
 *   flag, and each confirm it runs records it (StageResult's `confirmFlagged`).
 * - `control`: on the skill's control case, FRESH_RUNS runs each of old, `minus` (old less every
 *   line of the case's file that is the case's line, trimmed, or less every line that is
 *   `--planted-line`: a rule written twice is still the rule, so each copy goes; arms.ts) and
 *   `twin` (old again, its own capture: the false-loss control, which must be no loss). Old
 *   against `minus` is the planted-loss control, which must be a loss. `--control-informative`
 *   runs the control though the skill has fewer than MIN_INFORMATIVE_CASES informative cases, so
 *   a loss of one case has its control: the same control case (the control rule's), planted line,
 *   arms and stage as without it, and a skill with no case to be the control case still gets no
 *   control run. The control stage it runs records it (StageResult's `controlInformative`).
 *   confirm and control need the screen finished, and make no run when the skill can get no verdict
 *   (fewer than MIN_INFORMATIVE_CASES informative cases, a planted-loss control that is no loss,
 *   or a false-loss control that is a loss), except `confirm --confirm-flagged` and
 *   `control --control-informative`.
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
 *   arm's text (`--old`, `--new`) holds in a form the arms do not rebuild (build.ts's
 *   `pathFormIn`: arms.ts's `pathFormFault`, or a directory) is dropped at screen too, before
 *   any model call, as `path-form: <arm>: <fault>`, and the rest of the file runs: build-cases
 *   checks the old text and only a `--new` it was given, so a case built before the cut had a
 *   text reaches the screen unchecked against it.
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
 *   unless `--allow-runners` names each, and then prints them as allowed. It prints each case's
 *   passes per arm and stage, and for a dropped case the stage that dropped it, why, and the
 *   passes of each stage it ran; each confirm of a case
 *   with more than one, and the pooled passes of a case whose three confirms are pooled; the drops
 *   by the judge controls (and by each control), by `model-not-served`, `unreadable-directory`,
 *   `legion-tool-undefined`, `listing-misses`, `listing-adds`, `injection-unrebuildable` (by
 *   kind), `path-form` (by arm), `reads-past-last-round` and `provider-refused`; the control case,
 *   and each dropped case it
 *   replaced; the skill's verdict and its reason (score.ts; `keeps` is printed "no loss detected
 *   by the registered rule"), with how many of its screened cases have a listing that could not be
 *   measured, how many installed names their recorded listing gave them (`fromRecorded`), and how
 *   many rest on the assumption that their session had Dispatch; the rule's
 *   sensitivity (`lossChance` at an old pass rate of 0.9 and 0.7 and drops of 0.1 to 0.4), the
 *   estimated false-loss rate at the cases run (`falseLossChance`),
 *   the spend (`spendLines`: the sum of what the case results record their calls cost, part by
 *   part, and the request log's total and its difference from that sum, part by part, each part
 *   with its cause, where the log exists), the peak memory, how many captures' system-prompt
 *   digests differ from their
 *   session's, how many cases carry the Envoy plugin's block of the skill, by where the answer
 *   that their session had Dispatch comes from (arms.ts's `dispatchOf`: a Legion role, the
 *   transcript, or the assumption; each capture that carries the block records it in its stage's
 *   `captures`), and, for the cases whose capture sends their session's recorded system prompt
 *   whole (a profile with a `systemPrompt`), how many of their runs read a skill outside the
 *   recorded listing, by skill and by how the read was answered (score.ts's `outsideListing`:
 *   `copy`, an installed copy; `absent`, the unknown-skill reply), a count that drops no case and
 *   changes no verdict. With `--reading-models`, it prints each model of the file with its
 *   sessions and whether the gateway serves it (the file's `served`), the sessions with no model
 *   and not
 *   queried, and the served models the screened cases ran on, each with its cases; a verdict's
 *   line then ends with the reading sessions it speaks for, on each served model its screened
 *   cases ran on (a model of the file counted under the served model of its id), and the ones it
 *   does not: on models the gateway does not serve, and on served models no screened case ran
 *   on; a skill the screen's refusal holds for gets no verdict. Without it, `report` says the
 *   reading sessions by model were not given, and its verdict names no models. A drop at confirm
 *   of a case its screen did not flag cannot happen, since only flagged cases are confirmed: a
 *   result that shows one fails `report`, and every stage command, naming the case (exit 1). A
 *   run any of whose confirms `confirm --confirm-flagged` made, or whose control `control
 *   --control-informative` made, gives the case results as any run does, and the verdict line
 *   `case result only; no skill verdict stands (<option>)`, naming each option that made one
 *   (`caseResultOnly`), and no skill verdict, but for `--merged`: it says the results are a
 *   merged set of all the skill's cases (each case's newest run; report does not check it), and
 *   a set whose only such stages are confirms `--confirm-flagged` made then gets the registered
 *   rule's verdict line where that rule reaches a loss or "no loss detected", ending with how
 *   many confirms, a case's earlier confirms too, the option made (`; 1 confirm made by
 *   --confirm-flagged`; `verdictLine`). Where the rule reaches no skill verdict, and in a set
 *   whose control `--control-informative` made, the line stays `caseResultOnly`'s. The other
 *   commands refuse `--merged` (exit 2). With `--store-only <file>` (attribute.ts's
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
 *   the judged reply of each saved run, its arms built from `--old` and `--new` and its installed
 *   copies read from its stage's saved capture under `--cache-dir`, or, where that capture is not
 *   saved, from the copies the case's saved profile installs (`profileInstalledCopies`, the cache
 *   entries a capture of it records), and prints and writes to `<out>/audit-mixed.json`, per
 *   case, stage and arm, where its copies came from, the runs judged at a mixed reply with an
 *   act, those it would continue now, those it would leave with no verdict, past the last round
 *   or undecided, each with its run number, blind id and session id: ids, counts and tool names.
 *   A case whose profile copy the cache does not hold or cannot read is not audited, and is listed
 *   under `skipped` with why; the other cases are audited.
 * - `continue-mixed` (`continueMixedCommand`) continues the saved runs of `--from`, a saved
 *   `--out`, that `audit-mixed` sorts `continues`, into a new `--out`: the cut review's rule counts a
 *   saved case only once those runs are continued. `--from` is only read, and `--out` may not be
 *   it. Before any model call it refuses a saved stage of another runner unless `--allow-runners`
 *   names it, as the stage commands do, and an `--only` line that names a stage no saved result
 *   holds. A judged run the rule continues is continued from its saved reply when its record holds
 *   every part its next request needs: the reply is the first of its run (`skillRounds` 0: a
 *   saved step drops earlier rounds' thinking and text), it has a session id, its arm's saved
 *   capture is on disk, and the deployment that answered it is known (the run's last
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
 * routing id, on each of its requests in the header `x-litellm-session-id` (gateway.ts's
 * ROUTING_HEADER), which the gateway pins a call's deployment by: the runs numbered n of every
 * arm of a case's stage share one, a random UUID drawn once for n in each stage command, so old's,
 * new's and none's run n (minus's and twin's in a control) go to one deployment, every arm alike,
 * and the run numbers fall on the deployments as their ids hash. A Responses body goes with
 * none. Each run records its routing id (`routingId`, null for none) and the deployment that
 * answered each of its requests (`deployments`, gateway.ts's `deploymentOf`: the first 8 hex
 * digits of `x-litellm-model-id`). `--route-id <arm>=<uuid>` (repeatable) is a diagnostic: every
 * run of that arm, in every round, goes with that id in place of its round's, and records so
 * (`routePinned`), while the arms it does not name keep their rounds' ids; a pin of an arm the
 * command does not run, or of an id that is not a UUID, is refused (exit 2). A replay answers a
 * reply's reads of skill files (the case's skill from the arm, another skill of the profile from
 * the copy its capture installed, context.ts's `installedCopies` of the capture's record under
 * `--cache-dir`, made again before the replays where the cache lacks it, capture.ts's
 * `restoreCopies`, which first refuses a capture whose record names other copies than the case's
 * profile now gives, cached or not), and each
 * run records each answered round's reads (`skillReads`) and is judged on those rounds' read calls
 * and the reply after them. A reply that reads skill files beside other calls is sorted by the
 * case's act (replay.ts's `caseAct`, `callPart`): with an act among them it is judged as it came
 * (the run's `mixedAct`); with an undecided call it gets no verdict and is run again, its reason
 * beginning `mixed-undecided` and naming those calls; with only preparation its reads are
 * answered and it goes back holding only them, the other calls recorded as left out (`leftOut`)
 * and never run. `report` counts each kind by stage and arm. A run whose reply after the last
 * answered round still only reads skill files, or reads them beside only preparation, gets no
 * verdict and is run again, its reason beginning `reads-past-last-round`; each case's
 * line and `report` count such runs by arm, and a stage left unscreened by an arm with such runs
 * drops its case under that reason, with the counts. A stage left unscreened in which the provider
 * refused, on a content policy (a stream error whose type ends in `_policy`, as `cyber_policy`),
 * every replay that got no verdict in each arm that lacks its judged runs can never be judged, and
 * drops its case as `provider-refused: provider refused every replay of <arms> (<policy>): <n>
 * refusals (<arm> <count>, ...)`, the refused arms joined by ` and `, and none named where every
 * arm of the stage is refused; an arm with its judged runs blocks nothing. A stage saved unscreened
 * in that shape before this rule, with no drop, is dropped so when a stage command reaches its case
 * again, from its saved runs and with no model call. A stage where an arm that lacks its runs has a
 * run with no verdict for another reason stays unscreened. A capture made for a stage shows the
 * case's session (capture.ts, "The harness strings"), whose head the capture reads once from its
 * store through the configuration's `psqlCommand` and saves under `<out>/sessions/`.
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
 * appended to `--request-log` (by default `<out>/requests.jsonl` for `behaviour` and `load`, and
 * the `--out` file's path with `.requests.jsonl` added for `read` and `control`), holding what the
 * call was for, when it was asked for and sent and the calls in flight then, the session id it
 * went with, the model, the thinking type and budget, max_tokens, the temperature, the effort,
 * and the reply's answering model, ids, gateway headers, cache read and write tokens, usage and
 * dollars at list price; never the key and never a message's text.
 *
 * Exit status: 0 done (a control held); 1 failed (a reader's every reply refused, a control that
 * did not hold, a call, a load test's task or an `--after-case` command that failed, or behaviour
 * results that show a drop no stage makes); 2 refused before any model call; 3 stopped (the budget
 * is spent, the peak memory passed 1 GiB, 1.5 GiB for `copies`, or the listing rule dropped most
 * of a load test's tasks); 4 stopped by build-cases' stop rule. The commands of the other files
 * return their own status, which `main` returns as it is.
 */
import { createHash, randomUUID } from "node:crypto";
import {
  appendFileSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
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
  ModelCountFile,
  modelId,
  readStoreOnly,
  type StoreOnly,
  storeOnlyHosts,
  storeOnlyWords,
} from "./attribute";
import { main as buildMain, fromRecordedIn, pathFormIn } from "./build";
import {
  type Capture,
  captureBody,
  capturePaths,
  gatewayHints,
  LEGION_TOOL_UNDEFINED,
  LegionToolUndefined,
  listingOf,
  restoreCopies,
  type SessionFacts,
  savedCapture,
  sessionFacts,
  sessionHeads,
  userTurnKey,
} from "./capture";
import { type Case, loadCases, pathReadFile, Thinking } from "./cases";
import {
  configFrom,
  counted,
  json,
  jsonLines,
  jsonTextLines,
  mustBeDirectory,
  parse,
  pastLimit,
  Refused,
  readJson,
  required,
  Stopped,
  schemaFaults,
  timeLimit,
  writeWhole,
} from "./common";
import { type SkillCutsConfig, storeNamed } from "./config";
import {
  ghSource,
  installedCopies,
  localSource,
  profileRecords,
  type RepoSource,
  UNREADABLE_DIRECTORY,
  UnreadableDirectory,
} from "./context";
import { main as copiesMain, spawnGh } from "./copies";
import { main as countSqlMain } from "./countsql";
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
  usdOf,
} from "./gateway";
import {
  type ArmReply,
  CONTROLS,
  ControlName,
  type Judged,
  judgeBlind,
  judgeControls,
  newSeed,
} from "./judge";
import {
  auditLoadMixed,
  Host,
  LOAD_MODEL,
  LOAD_THINKING,
  runLoad,
  type TaskAudit,
  TEST_SETUP_LABEL,
  testSetupOption,
} from "./load";
import { procPeakRssBytes, startPeakRssSampling } from "./memory";
import {
  ATTACK_PLANTED_LINE,
  type Control,
  loadPrompt,
  plantControl,
  READER_NAMES,
  READERS,
  ReaderFailed,
  type ReaderName,
  runControl,
  runReader,
  SameJobInput,
} from "./readers";
import {
  AnsweredRead,
  type Boundary,
  type CallPart,
  caseAct,
  FurtherRoundLimit,
  LeftOutCall,
  MAX_SKILL_ROUNDS,
  MIXED_UNDECIDED,
  type MixedRule,
  NotResumable,
  partTools,
  READS_PAST_LAST_ROUND,
  ReadsPastLastRound,
  type Replayed,
  readsWords,
  replayInterleaved,
  replyBoundary,
  resumeReplay,
  type SkillRead,
  type SortedCall,
  sendOrder,
  UndecidedReply,
} from "./replay";
import {
  type CaseDrop,
  type CaseFacts,
  type CaseScore,
  FLAG_GAP,
  FRESH_RUNS,
  FURTHER_ROUND_LIMIT,
  INFORMATIVE_GAP,
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
  MAX_EXTRA_RUNS,
  MAX_SCREENED_CASES,
  type MadeConfirm,
  MIN_INFORMATIVE_CASES,
  outsideListing,
  outsideListingWords,
  POOLED_CONFIRMS,
  RunRecord,
  SCREEN_RUNS,
  Arm as ScoreArm,
  type SkillScore,
  STAGE_ARMS,
  STAGE_RUNS,
  Stage,
  StageState,
  scoreSkill,
  screeningDone,
  servedScreeningRefusal,
  skillVerdict,
  stageOutcome,
} from "./score";

/** The longest one try of a model call may take: a reply with a long thinking budget is slow. */
const CALL_TIMEOUT_MS = 600_000;
/** The model calls in flight at once, unless `--concurrency` says otherwise. */
const DEFAULT_CONCURRENCY = 8;
/** The most the runner's peak resident memory may reach: a case that ends above it stops the run
 *  (exit 3). */
const MEMORY_LIMIT_BYTES = 1024 ** 3;
/** The longest one `--after-case` command may run. It is the user's command, so its time is
 *  theirs; what it is for, a copy of the case's results file elsewhere, took 59 ms on the pod for
 *  a file of 2.2 MB (the largest results file the efficiency review found was 2.1 MB; measured
 *  2026-11-23), and 10 minutes leaves room for a copy to another machine. */
export const AFTER_CASE_TIMEOUT_MS = 600_000;
/** The request log's name: `<out>/requests.jsonl` for `behaviour` and `load`, and the `--out`
 *  file's path with `.requests.jsonl` added for `read` and `control`, unless `--request-log`
 *  names one. */
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

/** Bytes as whole MiB. */
function mib(bytes: number): number {
  return Math.round(bytes / 2 ** 20);
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

/** `skill-cuts control <kind>`. */
async function controlCommand(argv: readonly string[], io: CliIo): Promise<number> {
  const [kind, ...rest] = argv;
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
  /** Each answered round's reads (replay.ts's AnsweredRead), in order, or null when the replay
   *  failed; absent from a run made before this field. */
  skillReads: z.array(z.array(AnsweredRead)).nullable().optional(),
  /** The calls each continued round left out (replay.ts's LeftOutCall: a reply that read skill
   *  files beside calls that only prepare the case's act, whose reads were answered and sent back
   *  alone), in order, or null when the replay failed; absent from a run made before this
   *  field. */
  leftOut: z.array(LeftOutCall).nullable().optional(),
  /** True where the reply its step ends on read skill files beside an act (replay.ts's
   *  `replyBoundary`: `acts`), so it was judged at a mixed reply; absent otherwise. */
  mixedAct: z.literal(true).optional(),
  /** The step's content (each answered round's read calls, then the reply after them, which for a
   *  run that read past the last round, or whose mixed reply was undecided, is the reply it ended
   *  on, as it came), or null when the replay failed. */
  reply: z.array(z.looseObject({ type: z.string() })).nullable(),
  /** What the run's replay and its judgement used, each call's usage and dollars. A replay that
   *  failed (`REPLAY_FAILED`) records none, though the rounds it had answered were paid. */
  replaySpent: z.array(SpentRecord),
  /** The deployment that answered each of the replay's requests, in the order sent (gateway.ts's
   *  `deploymentOf`: the first 8 hex digits of its `x-litellm-model-id`, or the first 8 of its
   *  SHA-256 where the id does not begin with 8 hex digits, "" where the answer carried none);
   *  empty when the replay failed, as `replaySpent` is; absent from a run made before this
   *  field. */
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

/** One stage of a case: `running` until score.ts's `stageOutcome` says `counted` or `unscreened`.
 *  `texts` are the digests of `--old` and `--new` it ran with, and `plantedLine` the line `minus`
 *  left out in place of the case's (control only). `sends` is SENDS. `usd` is what its calls cost
 *  at list price: the gateway's spend while the stage ran, in each process that ran it, so its
 *  runs' replays and judgements and the calls no run records (its captures' hint calls, the
 *  rounds a replay that then failed had answered, a reply whose judgement the budget refused);
 *  never the calls of a round stopped before it was saved. `runner` is the runner that started it
 *  (`runnerDigest`), absent in a stage started before the runner was recorded; each of its runs
 *  records its own. `confirmFlagged` is true in a confirm that `behaviour
 *  confirm --confirm-flagged` ran, in any of its rounds, and absent otherwise; `controlInformative`
 *  is true in a control that `behaviour control --control-informative` ran, in any of its rounds,
 *  and absent otherwise: report gives no skill verdict over either (`caseResultOnly`), but for a
 *  merged set's confirms with `--merged` (`verdictLine`).
 *  `continued` is set on a stage `behaviour continue-mixed` wrote from a saved one: the saved
 *  `--out` it came from (`from`, as an absolute path), how many rounds the saved stage held
 *  (`savedRounds`), so a run of a later round that names no ContinueMixedRun is a fresh run made
 *  after it, and whether a run of the command selected it (`selected`: `--only` named it, or no
 *  `--only` was given), so its saved runs were continued, where false they were all kept; absent
 *  on every other stage. */
const StageResult = z.strictObject({
  state: StageState,
  sends: z.literal(SENDS),
  texts: z.strictObject({ old: z.string(), new: z.string() }),
  plantedLine: z.string().nullable(),
  captures: z.record(z.string(), CaptureNote),
  seeds: z.array(z.string()),
  runs: z.array(Run),
  usd: z.number(),
  runner: z.string().optional(),
  confirmFlagged: z.literal(true).optional(),
  controlInformative: z.literal(true).optional(),
  continued: z
    .strictObject({
      from: z.string(),
      savedRounds: z.int().nonnegative(),
      selected: z.boolean(),
    })
    .optional(),
});
type StageResult = z.infer<typeof StageResult>;

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
/** The arms whose texts a path-form drop checks: the two the screen and the confirm run. */
const PATH_FORM_ARMS = ["old", "new"] as const;

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

/** The runner that made `run` of stage `stage`: the run's own `runner`, else (a run made before
 *  runs recorded it) its stage's, else UNRECORDED_RUNNER. */
function runRunner(run: Run, stage: StageResult): string {
  return run.runner ?? stage.runner ?? UNRECORDED_RUNNER;
}

/** Each runner that made runs of `stage`, with how many it made (`runRunner`). A stage with no
 *  run counts under its own runner, else UNRECORDED_RUNNER, with 0 runs. */
function stageRunners(stage: StageResult): Map<string, number> {
  const runs = new Map<string, number>();
  for (const run of stage.runs) {
    const runner = runRunner(run, stage);
    runs.set(runner, (runs.get(runner) ?? 0) + 1);
  }
  if (runs.size === 0) runs.set(stage.runner ?? UNRECORDED_RUNNER, 0);
  return runs;
}

/** Where the captures of `arm` are saved under `out`: one directory per arm and text. `texts` are
 *  the digests of `--old` and `--new` (`textDigest`), so only `minus`'s copy is hashed here;
 *  `twin` holds the old text. */
export function armCaptureDir(
  out: string,
  arm: Arm,
  texts: { readonly old: string; readonly new: string }
): string {
  const text =
    arm.skillDir === undefined
      ? "none"
      : arm.name === "old" || arm.name === "new"
        ? texts[arm.name]
        : arm.name === "twin"
          ? texts.old
          : textDigest(arm.skillDir);
  return path.join(out, "captures", `${arm.name}-${text}`);
}

/** The key of case `id` in a scratch path: the first 16 hex digits of the SHA-256 of its id. */
function caseKey(id: string): string {
  return createHash("sha256").update(id).digest("hex").slice(0, 16);
}

/** Where the arms of case `id` are built under the runner's scratch `root` (buildArm's
 *  `scratch`): runStage's, and continue-mixed's, which builds them as runStage does. */
function armsScratch(root: string, id: string): string {
  return path.join(root, "arms", caseKey(id));
}

/** The chance of each count, 0 to `runs`, of `runs` runs that each pass with chance `p`. */
function binomial(runs: number, p: number): number[] {
  const chances: number[] = [];
  let ways = 1;
  for (let k = 0; k <= runs; k += 1) {
    chances.push(ways * p ** k * (1 - p) ** (runs - k));
    ways = (ways * (runs - k)) / (k + 1);
  }
  return chances;
}

/** From the chance of each count, the chance of each count or fewer. */
function atMost(chances: readonly number[]): number[] {
  let sum = 0;
  return chances.map((chance) => {
    sum += chance;
    return sum;
  });
}

/** The chance that a count of `runs` runs that each pass with chance `a` is at least `gap` more
 *  than a count of `runs` runs that each pass with chance `b`. */
function chanceAhead(runs: number, a: number, b: number, gap: number): number {
  const ahead = binomial(runs, a);
  const behind = atMost(binomial(runs, b));
  let sum = 0;
  for (let k = gap; k <= runs; k += 1) sum += (ahead[k] ?? 0) * (behind[k - gap] ?? 0);
  return sum;
}

/** Pass rates of the three texts a case runs under: the old text, the cut's and no skill. */
interface PassRates {
  readonly old: number;
  readonly cut: number;
  readonly none: number;
}

/**
 * The chance that the registered rule calls a loss in one informative case, when each run of the
 * old text passes with chance `old`, of the cut's text with `cut` and of no skill with `none`, all
 * runs apart. The case is flagged in the screen (o − n ≥ FLAG_GAP) given that it is informative
 * (o − z ≥ INFORMATIVE_GAP), o, n and z counts of SCREEN_RUNS runs, and then is a loss in the
 * confirm (O − N ≥ LOSS_GAP), O and N counts of FRESH_RUNS fresh runs; exact binomial:
 * P(o − n ≥ FLAG_GAP and o − z ≥ INFORMATIVE_GAP) / P(o − z ≥ INFORMATIVE_GAP) × P(O − N ≥
 * LOSS_GAP). Throws for rates under which no case is ever informative.
 */
export function lossChance(rates: PassRates): number {
  const old = binomial(SCREEN_RUNS, rates.old);
  const cut = atMost(binomial(SCREEN_RUNS, rates.cut));
  const none = atMost(binomial(SCREEN_RUNS, rates.none));
  let informative = 0;
  let flagged = 0;
  for (let k = 0; k <= SCREEN_RUNS; k += 1) {
    const chance = (old[k] ?? 0) * (none[k - INFORMATIVE_GAP] ?? 0);
    informative += chance;
    flagged += chance * (cut[k - FLAG_GAP] ?? 0);
  }
  if (informative === 0) {
    throw new Error(
      `no case is informative when the old text passes ${rates.old} and no skill ${rates.none} of the runs`
    );
  }
  return (flagged / informative) * chanceAhead(FRESH_RUNS, rates.old, rates.cut, LOSS_GAP);
}

/** An estimate of the chance that the registered rule calls a loss in one informative case when
 *  the cut's text is the old text: lossChance with both texts passing at p = (old + new + 1) /
 *  (2 × SCREEN_RUNS + 2), the case's old and cut screen passes pooled with a Laplace term, and no
 *  skill at q = (none + 1) / (SCREEN_RUNS + 2). */
export function falseLossChance(passes: { old: number; new: number; none: number }): number {
  const p = (passes.old + passes.new + 1) / (2 * SCREEN_RUNS + 2);
  const q = (passes.none + 1) / (SCREEN_RUNS + 2);
  return lossChance({ old: p, cut: p, none: q });
}

/** The report's sensitivity table: the old text's pass rates, the drops of the cut's text from
 *  them, and the no-skill pass rate. */
const SENSITIVITY = { old: [0.9, 0.7], drops: [0.1, 0.2, 0.3, 0.4], none: 0.1 } as const;

// `behaviour`: running the stages.

/** A behaviour command's cases and where it writes. `servedModels` are the configuration's, and
 *  `readingModels` the skill's reading sessions by model that `--reading-models` names, with its
 *  file (undefined without it). `allowRunners` are the runners `--allow-runners` names, whose
 *  saved stages a stage command takes as this runner's, and whose results report reports with
 *  others' (none without it). `storeOnly` is the skill's counted and store-only sessions that
 *  `--store-only` names (report only; undefined without it). `merged` is whether `--merged` is
 *  given: the results are a merged set of all the skill's cases (report only; `verdictLine`). */
interface Behaviour {
  readonly io: CliIo;
  readonly command: string;
  readonly out: string;
  readonly casesFile: string;
  readonly cases: readonly Case[];
  readonly servedModels: readonly string[];
  readonly readingModels: { readonly file: string; readonly count: ModelCount } | undefined;
  readonly allowRunners: readonly string[];
  readonly storeOnly: StoreOnly | undefined;
  readonly merged: boolean;
}

/** What a stage command runs with. `gateway` counts the spend; `replays`, `judge` and the capture
 *  send through it, each logged. `source` reads the context's repositories, and counts its GitHub
 *  reads; `cacheDir` holds the context's builds, among them each installed skill a replay answers
 *  a read of another skill from (context.ts's `installedCopies`). `runner` is this runner's code
 *  (`runnerDigest`), which each stage it starts and each run it makes records; `again` the cases
 *  `--again` names, and `confirmFlagged` whether `--confirm-flagged` is given (both confirm
 *  only); `controlInformative` whether `--control-informative` is given (control only);
 *  `routePins` the routing id `--route-id` pins each arm it names to (none without it). */
interface Runner extends Behaviour {
  readonly dirs: ArmDirs;
  readonly texts: { readonly old: string; readonly new: string };
  readonly scratch: string;
  readonly cacheDir: string;
  readonly gateway: Gateway;
  readonly replays: Gateway;
  readonly judge: Gateway;
  readonly source: RepoSource;
  capture(c: Case, arm: Arm): Promise<Capture>;
  readonly afterCase: string | undefined;
  readonly afterCaseFailures: string[];
  readonly runner: string;
  readonly again: readonly string[];
  readonly confirmFlagged: boolean;
  readonly controlInformative: boolean;
  readonly routePins: ReadonlyMap<ArmName, string>;
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
 *  (its stage records no `sends`, SENDS). */
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
    runs: stage.runs.map((run) => ({ ...run, reply: null, skillReads: null, leftOut: null })),
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

/** The stage that dropped `result`: its `droppedAt`, or for a result written before that was
 *  recorded, the stage its runs show: screen when its screen runs are not counted, else confirm
 *  when its screen flagged it and it has no confirm runs, else control. */
function dropStage(result: CaseResult): Stage {
  if (result.droppedAt !== undefined) return result.droppedAt;
  const screen = result.stages.screen;
  const outcome =
    screen === undefined ? undefined : stageOutcome(screen.runs, result.caseId, "screen");
  if (outcome?.state !== "counted") return "screen";
  return isFlagged(outcome.passes) && result.stages.confirm === undefined ? "confirm" : "control";
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

/** Case `caseId`'s stage at `place` as `--only` names it (`onlyStages`): `<case id>\t<stage>`. */
function stageKey(caseId: string, place: Pick<StagePlace, "stage" | "confirm">): string {
  return `${caseId}\t${placeWords(place)}`;
}

/** The texts of `stage` (or of a runner's `texts`) as score.ts's MadeConfirm holds them: the two
 *  digests in one string. */
const textsOf = (stage: { readonly texts: { readonly old: string; readonly new: string } }) =>
  `${stage.texts.old} ${stage.texts.new}`;

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

/** The drop at screen of case `c` when an arm's text of `dirs` (old, new) holds a file the case
 *  reads whole by path from its copy (`pathCopy`, cases.ts's `pathReadFile`) in a form the arms
 *  do not rebuild (build.ts's `pathFormIn`: arms.ts's `pathFormFault`, or a directory), on which
 *  buildArm would throw at the capture; undefined when it holds none. Its reason is
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
  const found = pathFormIn(files, dirs);
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
 *  that has one (`caseDrop`), and each case's confirms with the runner and texts of each. */
function caseFacts(b: Behaviour, results: ReadonlyMap<string, CaseResult>): Map<string, CaseFacts> {
  const facts = new Map<string, CaseFacts>();
  for (const c of b.cases) {
    const result = results.get(c.id);
    const drop = caseDrop(c, result);
    const confirms: MadeConfirm[] = (result === undefined ? [] : confirmStages(result)).map(
      (stage) => ({
        runner: stage.runner ?? null,
        texts: textsOf(stage),
        outcome: stageOutcome(stage.runs, c.id, "confirm"),
      })
    );
    facts.set(c.id, { ...(drop === undefined ? {} : { drop }), confirms });
  }
  return facts;
}

/** The score of `b`'s skill from `results` (score.ts's `scoreSkill`, with `caseFacts`). */
function scored(b: Behaviour, results: ReadonlyMap<string, CaseResult>): SkillScore {
  return scoreSkill(runRecords(results.values()), caseFacts(b, results));
}

/** Why the screen of `b`'s skill is not finished, or undefined when it is: each case is dropped
 *  or has its screen counted or unscreened, or screening stopped before it at the case cap. */
function screenUnfinished(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string | undefined {
  const dropped = new Set(
    score.cases.flatMap((each) => (each.drop === undefined ? [] : [each.caseId]))
  );
  for (const c of b.cases) {
    if (dropped.has(c.id)) continue;
    const result = results.get(c.id);
    const state = result?.stages.screen?.state;
    if (state === "counted" || state === "unscreened") continue;
    if (result === undefined && screeningDone(score)) continue;
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
  // Too few informative cases is the reason whatever the control is; with enough, an unscreened
  // control is no control loss, and its reason says why.
  return control?.state === "unscreened" && score.informative >= MIN_INFORMATIVE_CASES
    ? `${outcome.reason}: its runs are unscreened`
    : outcome.reason;
}

/** The drop of a case whose model the gateway does not serve, as its drop begins and the report
 *  counts it. */
const MODEL_NOT_SERVED = "model-not-served";

/** The model of `servedModels` (the configuration's, each `provider/model`) that serves `model`:
 *  the one whose id, its text after its last `/`, is `model`'s; or undefined when none is. */
function servedModel(model: string, servedModels: readonly string[]): string | undefined {
  return servedModels.find((served) => modelId(served) === modelId(model));
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

/** Refuses a stage of case `caseId` that was run with other skill texts or another planted line
 *  than this run's: its runs and this run's would be of different arms. */
function checkSameTexts(r: Runner, caseId: string, stage: Stage, existing: StageResult): void {
  const plantedLine = stage === "control" ? (r.dirs.plantedLine ?? null) : null;
  if (sameTexts(r, existing) && existing.plantedLine === plantedLine) {
    return;
  }
  throw new Refused(
    `case ${caseId}'s ${stage} runs in ${caseResultFile(r.out, caseId)} were made with other skill texts (old ${existing.texts.old}, new ${existing.texts.new}, planted line ${JSON.stringify(existing.plantedLine)}) than this run's (old ${r.texts.old}, new ${r.texts.new}, planted line ${JSON.stringify(plantedLine)}): use the same --old, --new and --planted-line, or another --out`
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
  const controls = await judgeControls(c, r.judge);
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
 *  `caseAct`); then every reply judged blind under `seed`. A replay that fails is a run with no
 *  verdict, its reason `<REPLAY_FAILED>: <why>` (REFUSED_REPLAY reads a content-policy refusal
 *  there), and so is one whose reply after the last answered round still only reads skill files,
 *  or reads them beside calls that only prepare (replay.ts's ReadsPastLastRound: its reason
 *  begins READS_PAST_LAST_ROUND), and one whose mixed reply is undecided (UndecidedReply: its
 *  reason begins MIXED_UNDECIDED), each of which keeps that run's reads, left-out calls, reply,
 *  spend and deployments; one the budget refused, or whose judgement the budget refused, is no
 *  run. The runs are numbered, within their arm, in the order they were sent, and each records
 *  this runner (`r.runner`), its routing id, whether that is its arm's `--route-id` pin, the
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
  phases: Phases
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
    { mixed: { kind: "case", act: caseAct(c) } }
  );
  // Each reply is judged under its arm and its place among its arm's replays of this round.
  const replies: ArmReply[] = sent.flatMap(({ arm, run, outcome }) =>
    outcome.status === "fulfilled"
      ? [{ arm, run, content: outcome.value.step.message.content }]
      : []
  );
  const judgeStarted = performance.now();
  phases.replaysMs += judgeStarted - replaysStarted;
  const judged = replies.length === 0 ? [] : (await judgeBlind(c, replies, r.judge, seed)).judged;
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
      // A run whose reply after the last answered round still reads skill files, or whose mixed
      // reply is undecided, keeps what it read, left out and spent.
      const past =
        outcome.reason instanceof ReadsPastLastRound || outcome.reason instanceof UndecidedReply
          ? outcome.reason
          : undefined;
      runs.push({
        ...base,
        run: next(),
        verdict: null,
        reason: past?.message ?? `${REPLAY_FAILED}: ${messageFor(outcome.reason)}`,
        id: null,
        skillRounds: past?.replayed.rounds ?? null,
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
      skillReads: step.reads.map((each) => [...each]),
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
 * this runner (`r.runner`), a confirm it runs or goes on with under `--confirm-flagged` records
 * that (`confirmFlagged`), and a control it runs or goes on with under `--control-informative`
 * records that (`controlInformative`). The runs numbered n of every arm go with one routing id, a
 * random UUID drawn for n the first time this call needs it (so a stage command started again
 * draws its own), and a pinned arm's runs with its `--route-id` pin (`r.routePins`): so round n
 * of runs, old's, new's and none's run n (minus's and twin's in a control), goes to the one
 * deployment its id pins, every arm alike, and the rounds fall on the deployments as their ids
 * hash. Throws Stopped, after saving, when the budget refused a call. The stage is the one `place`
 * names (StagePlace): the case's latest of its kind by default, or one of its earlier confirms,
 * whose runs `behaviour continue-mixed` gives fresh runs in place of those it runs again. A stage
 * whose arms lack no run but those that wait for the next decision (score.ts's
 * FURTHER_ROUND_LIMIT) is returned running, with no round started for them.
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
    plantedLine: stage === "control" ? (r.dirs.plantedLine ?? null) : null,
    captures: {},
    seeds: [],
    runs: [],
    usd: 0,
    runner: r.runner,
  };
  if (r.confirmFlagged) state = { ...state, confirmFlagged: true };
  if (r.controlInformative && stage === "control") state = { ...state, controlInformative: true };
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

  const scratch = armsScratch(r.scratch, c.id);
  const arms = new Map<ArmName, { arm: Arm; capture: Capture }>();
  const captures = { ...state.captures };
  const readsBefore = r.source.reads?.() ?? { calls: 0, ms: 0 };
  // The GitHub reads the captures made, counted into `phases`.
  const countReads = (): void => {
    const readsAfter = r.source.reads?.() ?? { calls: 0, ms: 0 };
    phases.github = {
      calls: readsAfter.calls - readsBefore.calls,
      ms: readsAfter.ms - readsBefore.ms,
    };
  };
  for (const name of STAGE_ARMS[stage]) {
    const started = performance.now();
    const arm = buildArm(c, name, r.dirs, scratch);
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
  save({ ...state, captures });

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
    const outcome = stageOutcome(state.runs, c.id, stage);
    if (outcome.state !== "running") {
      update({ ...state, state: outcome.state });
      const dropped =
        outcome.state === "unscreened"
          ? (readsPastDrop(state.runs, stage) ?? refusedDrop(state.runs, stage))
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
      phases
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
 *  the run when the peak is above MEMORY_LIMIT_BYTES. Returns the result as written. */
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
  if (peak > MEMORY_LIMIT_BYTES) {
    throw new Stopped(
      `the runner's peak resident memory, ${mib(peak)} MiB, is above 1 GiB, so the run stops after case ${result.caseId}, whose results are written to ${file}`
    );
  }
  return written;
}

/** `behaviour screen`. A saved screen of another runner refuses the command before any model call
 *  (`checkRunners`). */
async function screenCommand(r: Runner): Promise<void> {
  say(
    r,
    `${r.cases.length} cases of the skill ${r.cases[0]?.skill} in ${r.casesFile}; old text ${r.texts.old}, new text ${r.texts.new}`
  );
  // Each case's result, read once here and kept as each case ends (only this process writes
  // <out>/cases/), for the score that says when screening stops: as the score reads it
  // (`forScore`), so no case's replies are held while the screen runs.
  const results = new Map([...readResults(r)].map(([id, result]) => [id, forScore(result)]));
  checkRunners(r, results.values(), "screen");
  const finished = async (result: CaseResult, phases: Phases): Promise<void> => {
    results.set(result.caseId, forScore(await finishCase(r, result, "screen", phases)));
  };
  for (const c of r.cases) {
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
      const score = scored(r, results);
      if (screeningDone(score)) {
        say(
          r,
          `screening stops before case ${c.id}: ${score.informative} informative and ${score.screened} screened cases (it stops at ${MIN_INFORMATIVE_CASES} informative or ${MAX_SCREENED_CASES} screened)`
        );
        return;
      }
      result = newResult(c);
      if (servedModel(c.model, r.servedModels) === undefined) {
        const dropped = `${MODEL_NOT_SERVED}: its model ${c.model} is none the gateway serves (the configuration's servedModels: ${r.servedModels.join(", ") || "none"})`;
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
  say(r, "every case of the file is screened");
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
  const texts = textsOf(r);
  const losses = confirmStages(result).flatMap((stage) => {
    if (stage.runner !== r.runner || textsOf(stage) !== texts) return [];
    const outcome = stageOutcome(stage.runs, id, "confirm");
    return outcome.state === "counted" ? [isLoss(outcome.passes)] : [];
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
 *  confirmed, and report gives no skill verdict over them (`caseResultOnly`) but where
 *  `--merged` says the results are a merged set of all the skill's cases and the registered rule
 *  reaches one (`verdictLine`). A flagged case's saved last confirm of another runner refuses the
 *  command before any model call (`checkRunners`); earlier confirms are pooled by
 *  `confirmScore`'s rule. */
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

/** `behaviour control`. None runs while the skill has fewer than MIN_INFORMATIVE_CASES informative
 *  cases, unless `--control-informative` is given: then the control case's control runs all the
 *  same, so a loss of one case has its control, and report gives no skill verdict over it
 *  (`caseResultOnly`). A skill with no case to be the control case (`noControl`, or no informative
 *  case at all) gets none either way. The control case's saved control of another runner refuses
 *  the command before any model call (`checkRunners`). */
async function controlStageCommand(r: Runner): Promise<void> {
  const { results, score } = screened(r, (s) => [s.controlCase]);
  const tooFew = skillVerdict({
    informative: score.informative,
    controlLoss: true,
    falseLoss: false,
    losses: 0,
  });
  const overridden = tooFew.verdict === null ? tooFew.reason : undefined;
  if (overridden !== undefined && !r.controlInformative) {
    say(r, `no control runs: the skill gets no verdict: ${overridden}`);
    return;
  }
  if (score.noControl !== undefined) {
    say(r, `no control runs: the skill gets no verdict: ${score.noControl}`);
    return;
  }
  const id = score.controlCase;
  // With no informative case the control rule chooses none; only --control-informative gets here.
  if (id === undefined) {
    say(r, "no control runs: no case is informative, so the skill has no control case");
    return;
  }
  if (r.controlInformative) {
    say(
      r,
      `--control-informative: the control case's control runs${overridden === undefined ? "" : `, though the skill gets no verdict (${overridden})`}; report gives the case results only, and no skill verdict`
    );
  }
  // The control case's own work starts here: reading and scoring every case's result is no part
  // of its stage.
  const phases = newPhases();
  const c = r.cases.find((each) => each.id === id);
  const result = results.get(id);
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

/** What a stage's counted passes say. */
function stageVerdict(stage: Stage, passes: Record<ArmName, number>): string {
  switch (stage) {
    case "screen":
      if (!isInformative(passes)) return "not informative";
      return isFlagged(passes) ? "informative, flagged" : "informative, not flagged";
    case "confirm":
      return isLoss(passes) ? "a loss" : "no loss";
    case "control":
      return `the planted-loss control (old against minus) is ${isControlLoss(passes) ? "a loss" : "no loss"}, the false-loss control (old against twin) is ${isFalseLoss(passes) ? "a loss" : "no loss"}`;
  }
}

/** Each arm of `stage` with how many of `runs`, a stage's runs, `matches`. */
function armRuns(
  runs: readonly Run[],
  stage: Stage,
  matches: (run: Run) => boolean
): { arm: ArmName; count: number }[] {
  return STAGE_ARMS[stage].map((arm) => ({
    arm,
    count: runs.filter((run) => run.arm === arm && matches(run)).length,
  }));
}

/** Whether `run` got no verdict because the reply after the last answered round still read skill
 *  files, alone or beside calls that only prepare (replay.ts's ReadsPastLastRound, whose reason
 *  begins READS_PAST_LAST_ROUND). */
function readsPast(run: Run): boolean {
  return run.verdict === null && run.reason.startsWith(READS_PAST_LAST_ROUND);
}

/** Counts of arms in words: `old 1, new 0, none 3`. */
function armCounts(counts: readonly { arm: ArmName; count: number }[]): string {
  return counts.map(({ arm, count }) => `${arm} ${count}`).join(", ");
}

/** Why a case whose `stage` is unscreened is dropped, from the stage's `runs`: each arm that lacks
 *  its judged runs after its extra runs, with how many of its runs read skill files past the last
 *  answered round, when it has one or more such runs; then every arm's count of them. Undefined
 *  when no arm that lacks its runs has one: the stage then stays unscreened. */
function readsPastDrop(runs: readonly Run[], stage: Stage): string | undefined {
  const past = armRuns(runs, stage, readsPast);
  const short = past.flatMap(({ arm, count }) => {
    const own = runs.filter((run) => run.arm === arm);
    const judged = own.filter((run) => run.verdict !== null).length;
    return judged < STAGE_RUNS[stage] && count > 0
      ? [
          `the ${arm} arm has ${judged} of its ${STAGE_RUNS[stage]} judged runs after ${own.length} runs, ${count} of which still read skill files after the last answered round, alone or beside calls that only prepare`,
        ]
      : [];
  });
  if (short.length === 0) return undefined;
  return `${READS_PAST_LAST_ROUND}: ${short.join("; ")} (runs that read past the last round: ${armCounts(past)})`;
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
 *  policy, from the stage's `runs`, as `readsPastDrop` reads an arm: each arm that lacks its
 *  judged runs (STAGE_RUNS) is refused when each of its runs with no verdict is a refusal
 *  (REFUSED_REPLAY), and an arm with its judged runs blocks nothing. The reason names the refused
 *  arms joined by ` and ` (no arm where every arm of the stage is refused), each policy in the
 *  order first seen, how many runs were refused, and each arm's count of them. Undefined when no
 *  arm lacks its runs, or one that does has no refusal or a run with no verdict for another
 *  reason: the stage then stays unscreened. */
function refusedDrop(runs: readonly Run[], stage: Stage): string | undefined {
  const refusals = runs.flatMap((run) => {
    const policy = run.verdict === null ? REFUSED_REPLAY.exec(run.reason)?.[1] : undefined;
    return policy === undefined ? [] : [{ arm: run.arm, policy }];
  });
  const byArm: { arm: ArmName; count: number }[] = [];
  const refused: ArmName[] = [];
  for (const arm of STAGE_ARMS[stage]) {
    const own = runs.filter((run) => run.arm === arm);
    const unjudged = own.filter((run) => run.verdict === null).length;
    const count = refusals.filter((each) => each.arm === arm).length;
    byArm.push({ arm, count });
    if (own.length - unjudged >= STAGE_RUNS[stage]) continue;
    if (count === 0 || count < unjudged) return undefined;
    refused.push(arm);
  }
  if (refused.length === 0) return undefined;
  const of = refused.length === STAGE_ARMS[stage].length ? "" : ` of ${refused.join(" and ")}`;
  const policies = [...new Set(refusals.map((each) => each.policy))].join(", ");
  return `${PROVIDER_REFUSED}: provider refused every replay${of} (${policies}): ${refusals.length} refusals (${armCounts(byArm)})`;
}

/** Case `result` dropped at `stage` from its saved runs, where that stage is recorded unscreened
 *  with no drop and the provider refused its arms that lack their runs (`refusedDrop`): a stage
 *  saved before that rule, which a stage command that reaches the case again drops with no model
 *  call. Undefined for any other result. */
function savedRefusal(result: CaseResult, stage: Stage): CaseResult | undefined {
  const saved = result.stages[stage];
  if (result.dropped !== null || saved?.state !== "unscreened") return undefined;
  const dropped = refusedDrop(saved.runs, stage);
  return dropped === undefined ? undefined : { ...result, dropped, droppedAt: stage };
}

/** Whether `run` waits for the next decision at continue-mixed's further-round limit: no verdict,
 *  its reason beginning score.ts's FURTHER_ROUND_LIMIT, so no fresh run takes its place. */
function waits(run: Run): boolean {
  return run.verdict === null && run.reason.startsWith(FURTHER_ROUND_LIMIT);
}

/** What continue-mixed made of `run`, as its report and its lines name it: run again fresh (a
 *  ContinueMixedRun of kind `run again`), waiting at the further-round limit (one of kind
 *  `continued` that `waits`), or continued (any other of kind `continued`); undefined for a run
 *  with no ContinueMixedRun. */
function continuedKind(run: Run): "run again" | "waiting" | "continued" | undefined {
  const mark = run.continueMixed;
  if (mark === undefined) return undefined;
  if (mark.kind === "run again") return "run again";
  return waits(run) ? "waiting" : "continued";
}

/** Case `caseId`'s stage `state` of `stage` as one line, under `label`: each arm's passes and what
 *  they say, and how many runs got no verdict, those that read skill files past the last answered
 *  round counted by arm; a stage not finished says what it still starts and, by arm, the runs
 *  that wait at continue-mixed's further-round limit (score.ts's FURTHER_ROUND_LIMIT). */
function stageLine(
  caseId: string,
  stage: Stage,
  state: StageResult | undefined,
  label: string = stage
): string {
  if (state === undefined) return `${label}: not run`;
  const outcome = stageOutcome(state.runs, caseId, stage);
  const unjudged = state.runs.filter((run) => run.verdict === null).length;
  const past = armRuns(state.runs, stage, readsPast);
  const pastWords = past.some(({ count }) => count > 0)
    ? `; ${READS_PAST_LAST_ROUND}: ${armCounts(past)}`
    : "";
  const extra = unjudged === 0 ? "" : ` (${unjudged} runs with no verdict, run again${pastWords})`;
  switch (outcome.state) {
    case "counted": {
      const passes = Object.entries(outcome.passes)
        .map(([arm, count]) => `${arm} ${count}/${STAGE_RUNS[stage]}`)
        .join(", ");
      return `${label}: ${passes}: ${stageVerdict(stage, outcome.passes)}${extra}`;
    }
    case "unscreened":
      return `${label}: unscreened: an arm lacks its ${STAGE_RUNS[stage]} judged runs after ${MAX_EXTRA_RUNS} extra runs${extra}`;
    case "running": {
      const toStart = Object.entries(outcome.toStart)
        .map(([arm, count]) => `${arm} ${count}`)
        .join(", ");
      const waiting = armRuns(state.runs, stage, waits);
      const waitWords = waiting.some(({ count }) => count > 0)
        ? `; waiting at the further-round limit for the next decision: ${armCounts(waiting)}`
        : "";
      return `${label}: not finished, still to start: ${toStart}${waitWords}`;
    }
  }
}

/** A case's pooled confirms (score.ts's `confirmScore`, counted with `pooled`) in words: the
 *  pooled passes of each arm, the gap, and whether it is a loss. */
function pooledWords(pooled: { old: number; new: number }, loss: boolean): string {
  const runs = FRESH_RUNS * POOLED_CONFIRMS;
  return `old ${pooled.old}/${runs}, new ${pooled.new}/${runs}, ${pooled.old - pooled.new} fewer: ${loss ? "a loss" : "no loss"}`;
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
          : textsOf(each) !== textsOf(last)
            ? " (at other texts, not counted)"
            : "";
      lines.push(stageLine(result.caseId, stage, each, `confirm ${n + 1}${note}`));
    }
    const confirm = score?.confirm;
    if (confirm?.state === "counted" && confirm.pooled !== undefined) {
      lines.push(`pooled: ${pooledWords(confirm.pooled, confirm.loss)}`);
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

/** The served models the screened (counted) cases of `score` ran on, each case on the served model
 *  of its model's id (`servedModel`), with its screened cases, most first, a tie by model. */
function screenedModels(b: Behaviour, score: SkillScore): { model: string; cases: number }[] {
  const cases = new Map<string, number>();
  for (const each of score.cases) {
    const c = b.cases.find((candidate) => candidate.id === each.caseId);
    if (c === undefined || each.screen.state !== "counted") continue;
    const model = servedModel(c.model, b.servedModels) ?? c.model;
    cases.set(model, (cases.get(model) ?? 0) + 1);
  }
  return [...cases]
    .map(([model, count]) => ({ model, cases: count }))
    .sort((x, y) => y.cases - x.cases || x.model.localeCompare(y.model));
}

/** The report's lines on the skill's reading sessions by model (`--reading-models`): each model
 *  of the file with its sessions and whether the gateway serves it, the sessions with no model and
 *  not queried, and the served models the screened cases ran on; or one line saying they were not
 *  given. */
function modelLines(b: Behaviour, score: SkillScore): string[] {
  if (b.readingModels === undefined) {
    return [
      "reading sessions by model: not given (no --reading-models), so the verdict names no models",
    ];
  }
  const { file, count } = b.readingModels;
  const ran = screenedModels(b, score)
    .map((item) => `${item.model} ${counted(item.cases, "case")}`)
    .join(", ");
  return [
    `reading sessions by model (--reading-models ${file}): ${counted(count.sessions, "session")}`,
    ...count.byModel.map(
      (item) =>
        `  ${item.model}: ${counted(item.sessions, "session")}, ${item.served ? "served" : "not served"} by the gateway`
    ),
    `  with no model (its file's head names none): ${count.noModel}; not queried (in no models query's rows): ${count.notQueried}`,
    `screened cases by the served model they ran on: ${ran || "none"}`,
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
 *  for, on each served model its screened cases ran on (a served model of the file counted under
 *  the one of its id), and those it does not: the sessions on models the gateway does not serve,
 *  and any on served models no screened case ran on. Empty without `--reading-models`. */
function speaksFor(b: Behaviour, score: SkillScore): string {
  const count = b.readingModels?.count;
  if (count === undefined) return "";
  const spoken = screenedModels(b, score).map(({ model }) => ({
    model,
    id: modelId(model),
    sessions: 0,
  }));
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
      : `; nor for the ${counted(otherServed, "session")} on served models no screened case ran on`)
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

/** The case-result-only causes of a run some of whose stages an option made that runs a stage
 *  whatever the skill can get, or undefined when none did: `flaggedConfirms`, the confirms
 *  `behaviour confirm --confirm-flagged` made (StageResult's `confirmFlagged`; a case's earlier
 *  confirms too), which ran whatever the skill's informative count and controls, and
 *  `informativeControl`, whether a control `behaviour control --control-informative` made
 *  (`controlInformative`) is among them, which ran though the skill has fewer than
 *  MIN_INFORMATIVE_CASES informative cases. Such stages give case results only: `line`, the
 *  verdict line, names each option that made one of `results`. */
function caseResultOnly(results: ReadonlyMap<string, CaseResult>):
  | {
      readonly line: string;
      readonly flaggedConfirms: number;
      readonly informativeControl: boolean;
    }
  | undefined {
  const all = [...results.values()];
  const flaggedConfirms = all.reduce(
    (made, result) =>
      made + confirmStages(result).filter((stage) => stage.confirmFlagged === true).length,
    0
  );
  const informativeControl = all.some(
    (result) => result.stages.control?.controlInformative === true
  );
  const options = [
    ...(flaggedConfirms > 0 ? ["--confirm-flagged"] : []),
    ...(informativeControl ? ["--control-informative"] : []),
  ];
  return options.length === 0
    ? undefined
    : {
        line: `case result only; no skill verdict stands (${options.join(", ")})`,
        flaggedConfirms,
        informativeControl,
      };
}

/** The skill's verdict as one line: the registered rule's (`ruleLine`), but for a run any of
 *  whose confirms, earlier ones too, records `confirmFlagged`, or whose control records
 *  `controlInformative`, which gets `caseResultOnly`'s line and no verdict. With `--merged`, the
 *  results are a merged set of all the skill's cases (each case's newest run, merged into one
 *  set), where a confirm `--confirm-flagged` made is the confirm stage the rule counts: so a set
 *  whose only such stages are those confirms, where the rule reaches a skill verdict over them
 *  (a loss or "no loss detected"), gets the rule's own line ending with how many confirms of the
 *  set, a case's earlier confirms too, the option made (`; 1 confirm made by
 *  --confirm-flagged`); where the rule reaches none, or a control records `controlInformative`,
 *  it keeps `caseResultOnly`'s line. */
function verdictLine(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): string {
  const byOption = caseResultOnly(results);
  if (byOption === undefined) return ruleLine(b, results, score).line;
  if (!b.merged || byOption.informativeControl) return byOption.line;
  const rule = ruleLine(b, results, score);
  return rule.verdict
    ? `${rule.line}; ${counted(byOption.flaggedConfirms, "confirm")} made by --confirm-flagged`
    : byOption.line;
}

/** The registered rule's line of the skill (score.ts), and whether it is a skill verdict, a loss
 *  or "no loss detected" (`verdict`), or else what the rule waits for or why it reaches none: a
 *  verdict ends with the reading sessions it speaks for (`speaksFor`), and a verdict or its reason
 *  with how many screened cases' listings could not be measured, took names from their recorded
 *  listing, and rest on the assumption that their session had Dispatch (`listingWords`). With
 *  `--reading-models`, a skill too few of whose reading sessions are on served models
 *  (`servedScreeningRefusal`) gets none. With `--store-only`, the reason of a skill with fewer
 *  than MIN_INFORMATIVE_CASES informative cases ends with how many of its counted sessions only
 *  the transcript store holds, so were never case sources (`storeOnlyWords`). */
function ruleLine(
  b: Behaviour,
  results: ReadonlyMap<string, CaseResult>,
  score: SkillScore
): { readonly line: string; readonly verdict: boolean } {
  const none = (line: string) => ({ line, verdict: false });
  const refusal =
    b.readingModels === undefined ? undefined : servedScreeningRefusal(b.readingModels.count);
  if (refusal !== undefined) return none(`none: ${refusal.reason}`);
  const unfinished = screenUnfinished(b, results, score);
  if (unfinished !== undefined) return none(`not yet: ${unfinished} (behaviour screen)`);
  const outcome = score.outcome;
  if (outcome === undefined) {
    const third = score.cases.flatMap((each) =>
      each.confirm?.state === "running" && each.confirm.third ? [each.caseId] : []
    );
    return none(
      score.control?.state === "running"
        ? "not yet: the control runs are not all made (behaviour control)"
        : third.length > 0
          ? `not yet: two confirms on one runner disagree in ${third.join(", ")}, so a third confirm of each is due (behaviour confirm)`
          : "not yet: the confirm runs of a flagged case are not all made (behaviour confirm)"
    );
  }
  const listing = listingWords(b, results, score);
  if (outcome.verdict === null) {
    const storeOnly =
      b.storeOnly !== undefined && score.informative < MIN_INFORMATIVE_CASES
        ? storeOnlyWords(b.storeOnly)
        : "";
    return none(`none: ${outcome.reason}${storeOnly}${listing}`);
  }
  const flagged = score.cases.filter((each) => each.flagged).length;
  const control = `the planted-loss control (case ${score.controlCase}) is a loss and its false-loss control is no loss`;
  if (outcome.verdict === "keeps") {
    return {
      line: `no loss detected by the registered rule: no confirmed case is a loss (${flagged} flagged of ${score.informative} informative), and ${control}${speaksFor(b, score)}${listing}`,
      verdict: true,
    };
  }
  const losses = (pooled: boolean) =>
    score.cases.flatMap((each) =>
      each.loss &&
      each.confirm?.state === "counted" &&
      (each.confirm.pooled !== undefined) === pooled
        ? [each.caseId]
        : []
    );
  const single = losses(false);
  const pooled = losses(true);
  const where = [
    ...(single.length === 0
      ? []
      : [
          `in ${single.join(", ")} the cut's text passes at least ${LOSS_GAP} fewer of ${FRESH_RUNS} fresh runs than the old text`,
        ]),
    ...(pooled.length === 0
      ? []
      : [
          `in ${pooled.join(", ")}, over ${POOLED_CONFIRMS} pooled confirms, at least ${LOSS_GAP * POOLED_CONFIRMS} fewer of ${FRESH_RUNS * POOLED_CONFIRMS}`,
        ]),
  ];
  return {
    line: `loss: ${where.join("; ")}, and ${control}${speaksFor(b, score)}${listing}`,
    verdict: true,
  };
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

/** The report's lines of where `stage`'s runs `runs` went: its arms' runs counted by the
 *  deployment that answered them (`<stage> deployments by arm: old <id> <n>, ...; new ...`,
 *  gateway.ts's `deploymentWords`), `not recorded` where no run records its deployments (results
 *  made before runs did), with how many runs do not where some do; and, where an arm's runs went
 *  with its `--route-id` pin, the pins and their cases (`<stage> routing pinned: old=<id>, ...`):
 *  such runs are a diagnostic, never a test result. */
function routingLines(stage: Stage, runs: readonly Run[]): string[] {
  const unrecorded = runs.filter((run) => run.deployments === undefined).length;
  const byArm = STAGE_ARMS[stage]
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
    const pins = STAGE_ARMS[stage].flatMap((arm) => [
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
function continuedLines(results: ReadonlyMap<string, CaseResult>): string[] {
  type Kind = "continued" | "waiting" | "run again" | "fresh" | "unchanged";
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
        const kind: Kind =
          continuedKind(run) ?? (run.round > continued.savedRounds ? "fresh" : "unchanged");
        const usd =
          run.continueMixed?.usd ??
          (kind === "fresh" ? usdOf(run.replaySpent) + usdOf(run.judgeSpent) : 0);
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
        return `${stage} ${armCounts(STAGE_ARMS[stage].map((arm) => ({ arm, count: count(arm) })))}`;
      })
      .join("; ");
  const spent = (kinds: readonly Kind[]) => {
    const own = rows.filter((row) => kinds.includes(row.kind));
    const parts = Stage.options.map(
      (stage) => `${stage} ${dollars(usdOf(own.filter((row) => row.stage === stage)))}`
    );
    return `${dollars(usdOf(own))} over ${counted(own.length, "run")} (${parts.join(", ")})`;
  };
  return [
    `continue-mixed: ${counted(stages, "case stage")} written from ${[...froms].join(", ")}, ${selected.length === stages ? "every one selected" : `${selected.length} of them selected by --only${selected.length === 0 ? "" : ` (${selected.join(", ")})`}, the others' runs all kept as saved`}`,
    `continue-mixed continued (a saved run judged at a mixed reply whose other calls only prepare, continued from that reply and judged again), runs by stage and arm: ${byStage("continued")}`,
    `continue-mixed waiting (${FURTHER_ROUND_LIMIT}: a continued run stopped with no verdict at the further-round limit, whose reply would be continued again, which no fresh run replaces until the next round is decided), runs by stage and arm: ${byStage("waiting")}`,
    `continue-mixed run again fresh (a saved run the rule gives no verdict, or whose continuation its record could not rebuild, failed, or another deployment answered), runs by stage and arm: ${byStage("run again")}`,
    `continue-mixed fresh runs (made by the stage's extra-run rule in place of the runs with no verdict), runs by stage and arm: ${byStage("fresh")}`,
    `continue-mixed unchanged (saved runs kept as they were), runs by stage and arm: ${byStage("unchanged")}`,
    `continue-mixed spend at list price: the continuations ${spent(["continued", "waiting", "run again"])}, their requests and judgements, a discarded one's among them; the fresh runs ${spent(["fresh"])}, their replays and judgements`,
  ];
}

/** `behaviour report`: prints the runners of the results (`runnerLine`, which refuses results of
 *  more than one runner unless told), each case's passes, the skill's verdict and the run's
 *  figures, its spend among them (`spendLines`, and `requestLogLines` for the request log
 *  `requestLog`). A dropped case's line gives the stage that dropped it, why, and each stage it
 *  ran. Its `outside recorded listing` line counts the runs of the cases whose capture sends
 *  their session's recorded system prompt whole that read a skill outside that listing
 *  (`outsideListing`): a count only, which drops no case and changes no verdict. Its
 *  `deployments by arm` line of each stage kind, and `routing pinned` line of a kind with pinned
 *  runs (`routingLines`), say where the runs went; they change no verdict either. Its
 *  `continue-mixed` lines (`continuedLines`), printed only for results continue-mixed wrote, count
 *  the runs of each kind it made, by stage and arm, and their spend apart. */
function reportCommand(b: Behaviour, requestLog: string): void {
  const results = readResults(b);
  const runners = runnerLine(b, results);
  const score = scored(b, results);
  const byCase = new Map(score.cases.map((each) => [each.caseId, each]));
  const lines = [`skill ${b.cases[0]?.skill}: ${b.cases.length} cases in ${b.casesFile}`, runners];
  for (const c of b.cases) {
    const result = results.get(c.id);
    const drop = byCase.get(c.id)?.drop;
    const ran = result === undefined ? [] : caseStageLines(result, byCase.get(c.id));
    const line =
      drop !== undefined
        ? `dropped at ${drop.stage}: ${drop.reason}; ${ran.length === 0 ? "screen: not run" : ran.join("; ")}`
        : ran.length === 0
          ? "not run"
          : ran.join("; ");
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
  // rebuild, by the arm its reason names (`pathFormDrop`).
  const formed = drops.filter((reason) => reason.startsWith(`${PATH_FORM}: `));
  const byArm = PATH_FORM_ARMS.map(
    (arm) =>
      `${arm} ${formed.filter((reason) => reason.startsWith(`${PATH_FORM}: ${arm}: `)).length}`
  ).join(", ");
  lines.push(
    `cases: ${score.screened} screened, ${score.informative} informative, ${flagged} flagged, ${losses} confirmed losses; ${drops.length} dropped (${byControls} by the judge controls, ${droppedAs(MODEL_NOT_SERVED)}, ${droppedAs(UNREADABLE_DIRECTORY)}, ${droppedAs(LEGION_TOOL_UNDEFINED)}, ${droppedAs(LISTING_MISSES)}, ${droppedAs(LISTING_ADDS)}, ${injected.length} ${INJECTION_UNREBUILDABLE} (${byKind}), ${formed.length} ${PATH_FORM} (${byArm}), ${droppedAs(READS_PAST_LAST_ROUND)}, ${droppedAs(PROVIDER_REFUSED)}), ${unscreened} unscreened, ${notRun} not run`
  );
  // Each arm's runs of each stage, the earlier confirms too, whose reply after the last answered
  // round still read skill files, alone or beside calls that only prepare: an arm that keeps the
  // agent reading acts differently.
  const runsByStage = Stage.options.map((stage) => [stage, kindRuns(results, stage)] as const);
  const pastByStage = runsByStage.map(
    ([stage, runs]) => `${stage} ${armCounts(armRuns(runs, stage, readsPast))}`
  );
  lines.push(
    `${READS_PAST_LAST_ROUND}: the runs whose reply after the last of ${MAX_SKILL_ROUNDS} answered rounds of skill reads still read skill files, alone or beside calls that only prepare, which get no verdict and are run again, by stage and arm: ${pastByStage.join("; ")}`
  );
  // Each arm's runs of each stage whose replay met a reply that read skill files beside other
  // calls (replay.ts's `replyBoundary`): continued past one whose other calls only prepare the
  // case's act, judged at one with an act, or left with no verdict by one with an undecided call.
  // One past the last round counts above, as reads-past-last-round.
  const mixedByStage = (holds: (run: Run) => boolean) =>
    runsByStage
      .map(([stage, runs]) => `${stage} ${armCounts(armRuns(runs, stage, holds))}`)
      .join("; ");
  lines.push(
    `mixed replies continued (the reply read skill files beside calls that only prepare the case's act: its reads answered and sent back alone, its other calls left out, never run), runs by stage and arm: ${mixedByStage((run) => (run.leftOut?.length ?? 0) > 0)}`,
    `mixed replies judged (the reply read skill files beside an act, and was judged as it came), runs by stage and arm: ${mixedByStage((run) => run.mixedAct === true)}`,
    `${MIXED_UNDECIDED}: the runs whose reply read skill files beside a call that is neither an act nor only preparation, which get no verdict and are run again, by stage and arm: ${mixedByStage((run) => run.verdict === null && run.reason.startsWith(MIXED_UNDECIDED))}`
  );
  // Where each stage kind's runs went: the deployment that answered each arm's runs, and any
  // arm whose runs went with a `--route-id` pin.
  lines.push(...runsByStage.flatMap(([stage, runs]) => routingLines(stage, runs)));
  // What `behaviour continue-mixed` made of the stages it wrote, and what that cost.
  lines.push(...continuedLines(results));
  const pooled = score.cases.flatMap(({ caseId, confirm }) =>
    confirm?.state === "counted" && confirm.pooled !== undefined
      ? [
          `${caseId} (confirms of old ${confirm.counted.map((each) => each.old).join(", ")} and new ${confirm.counted.map((each) => each.new).join(", ")} of ${FRESH_RUNS} each; pooled ${pooledWords(confirm.pooled, confirm.loss)})`,
        ]
      : []
  );
  if (pooled.length > 0) {
    lines.push(
      `pooled confirms (two confirms at one case's texts on one runner disagreed, so a third ran, and the case is a loss when the cut's text passes at least ${LOSS_GAP * POOLED_CONFIRMS} fewer of the ${FRESH_RUNS * POOLED_CONFIRMS} pooled runs than the old text): ${pooled.join("; ")}`
    );
  }
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
  lines.push(...modelLines(b, score), ...storeOnlyLines(b));
  lines.push(`verdict: ${verdictLine(b, results, score)}`);

  const percent = (x: number) => `${(100 * x).toFixed(2)}%`;
  lines.push(
    `sensitivity of the registered rule: the chance that it calls a loss in one informative case whose no-skill arm passes ${SENSITIVITY.none} of its runs, by the old text's and the cut's pass rates (exact binomial)`
  );
  for (const old of SENSITIVITY.old) {
    const row = SENSITIVITY.drops.map((drop) => {
      const cut = old - drop;
      return `new ${cut.toFixed(1)} ${percent(lossChance({ old, cut, none: SENSITIVITY.none }))}`;
    });
    lines.push(`  old ${old.toFixed(1)}: ${row.join(", ")}`);
  }

  const chances = score.cases.flatMap((each) =>
    each.informative && each.screen.state === "counted"
      ? [{ id: each.caseId, chance: falseLossChance(each.screen.passes) }]
      : []
  );
  const rate = 1 - chances.reduce((none, { chance }) => none * (1 - chance), 1);
  lines.push(
    `false-loss rate at the cases run (an estimate): ${percent(rate)}, the chance that the registered rule calls a loss in one of the ${chances.length} informative cases when the cut's text is the old text; each case's chance is exact binomial given that it is informative, with the old and the cut's texts both passing at (old + new + 1)/${2 * SCREEN_RUNS + 2} of their runs and no skill at (none + 1)/${SCREEN_RUNS + 2}, from its screen passes, and the cases are combined as independent, 1 − Π(1 − chance)${chances.length === 0 ? "" : ` (${chances.map(({ id, chance }) => `${id} ${percent(chance)}`).join(", ")})`}`
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
  const answers = [...dispatchAnswers(results).values()];
  const from = (answer: DispatchFrom) => answers.filter((each) => each === answer).length;
  lines.push(
    `dispatch: ${answers.length} cases carry the Envoy plugin's block of the skill: ${from("assumed")} rest on the assumption that their session had Dispatch, ${from("legion-role")} take it from a Legion role, ${from("transcript")} from their transcript (arms.ts's dispatchOf)`
  );
  // The runs of each case whose capture sends its session's recorded system prompt whole (its
  // profile holds the prompt, capture.ts's `systemPrompt` choice) that read a skill outside the
  // recorded listing, from the reads each run records: a count only, which drops no case and
  // changes no verdict.
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
                stage.runs.map((run) => run.skillReads?.flat())
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

/** The installed copies of `arm`'s profile's repository skills (context.ts's `profileRecords`, the
 *  skills a context of the arm installs from their repositories): each under the name the profile
 *  lists it under, from its cache entry `<cacheDir>/skills/<key>`, exactly as `installedCopies`
 *  gives a saved capture record's copies, since a capture's record names those copies. What a
 *  read of another skill is answered from where the arm's capture is not saved. Exported for
 *  cli.test.ts. */
export function profileInstalledCopies(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills">,
  cacheDir: string
): InstalledCopy[] {
  return installedCopies({ skills: profileRecords(arm) }, cacheDir);
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
  texts: { readonly old: string; readonly new: string }
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

/** The skill directories the arms of saved stage `state` are built from: `--old` and `--new` of
 *  `dirs`, with the stage's own planted line where it has one. */
function stageArmDirs(
  dirs: { readonly old: string; readonly new: string },
  state: Pick<StageResult, "plantedLine">
): ArmDirs {
  return {
    old: dirs.old,
    new: dirs.new,
    ...(state.plantedLine === null ? {} : { plantedLine: state.plantedLine }),
  };
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
  label: string
): RunSort {
  const answered = run.skillReads?.flat().length ?? (run.skillRounds === 0 ? 0 : null);
  if (run.reply === null) return { kind: "noReply" };
  if (answered === null) return { kind: "unrecorded" };
  let boundary: Boundary;
  try {
    boundary = replyBoundary(run.reply.slice(answered), arm, copies, rule);
  } catch (error) {
    throw new Error(`${label} run ${run.run}: ${messageFor(error)}`);
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
 *  `auditMixedCommand`). `confirm` numbers a case's confirms from 1, oldest first; `copiesFrom`
 *  says where the installed copies its runs were sorted with came from (CopiesFrom). */
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
 * (`sortRun`: `replyBoundary` under the case's act, `caseAct`) applied to the judged reply of each
 * saved run of each stage of `b`'s cases, earlier confirms included. Each arm is built from `dirs`
 * (`--old`, `--new`, and a control's planted line from its stage) in a scratch directory of its
 * own under `scratchRoot`, removed after, and answers a read of another skill from the installed
 * copies its stage's saved capture names under `cacheDir`, which no read restores; where that
 * capture is not saved, from the copies the case's saved profile installs
 * (`profileInstalledCopies`), the cache entries a capture of the arm records. A stage run with
 * other texts than `dirs` is skipped and listed, and so is a case one of whose arms' copies come
 * from its profile where the cache does not hold one of them or cannot read it (`unreadableCopy`):
 * no stage of that case is audited, and the audit goes on with the other cases. For each case,
 * stage and arm it prints, and writes as JSON to `<out>/audit-mixed.json`, which it names, where
 * its copies came from (`copiesFrom`), the runs and the judged runs, and each run whose judged
 * reply read skill files beside other calls: judged at it, an act among them (`acts`); continued
 * now, its other calls only preparation (`continues`); no verdict, as such a reply after the last
 * round (`pastLastRound`); or no verdict, a call undecided (`unresolved`); and counts the runs
 * whose reply only reads skill files (`skillRound`), holds no such mix (`ends`), is not saved
 * (`noReply`, a replay that failed), or whose answered rounds a run saved before `skillReads`
 * cannot tell (`unrecorded`). Its output holds ids, counts, tool names and paths, never a
 * request's or a reply's text. Throws as `replyBoundary` throws for a read of a skill whose
 * installed copy the cache lacks, naming the run.
 */
function auditMixedCommand(
  b: Behaviour,
  dirs: { readonly old: string; readonly new: string },
  cacheDir: string,
  scratchRoot: string
): void {
  const results = readResults(b);
  const texts = { old: textDigest(dirs.old), new: textDigest(dirs.new) };
  mkdirSync(scratchRoot, { recursive: true });
  const scratch = mkdtempSync(path.join(scratchRoot, "audit-mixed-"));
  const audits: ArmAudit[] = [];
  const skipped: AuditSkip[] = [];
  /** The audit of each arm of each stage of case `c`'s `result`, or, where an arm's copies come
   *  from its profile and one cannot be read, the skip of the whole case. */
  const auditCase = (c: Case, result: CaseResult): ArmAudit[] | AuditSkip => {
    const found: ArmAudit[] = [];
    const rule: MixedRule = { kind: "case", act: caseAct(c) };
    const armScratch = path.join(scratch, caseKey(c.id));
    // Each saved capture the case's stages share (one per arm and text, `armCaptureDir`), parsed
    // once, by its record file.
    const captures = new Map<string, Capture | undefined>();
    for (const place of stagePlaces(result)) {
      const state = place.get(result);
      if (state === undefined || state.runs.length === 0) continue;
      const { stage, confirm } = place;
      const where = { caseId: c.id, stage, ...(confirm === undefined ? {} : { confirm }) };
      if (!sameTexts({ texts }, state)) {
        skipped.push({
          ...where,
          why: `run with the texts old ${state.texts.old} and new ${state.texts.new}, not --old ${texts.old} and --new ${texts.new}`,
        });
        continue;
      }
      const armDirs = stageArmDirs(dirs, state);
      for (const name of STAGE_ARMS[stage]) {
        const runs = state.runs.filter((run) => run.arm === name);
        if (runs.length === 0) continue;
        const arm = buildArm(c, name, armDirs, armScratch);
        const files = stageCaptureFiles(b.out, c.id, arm, state, texts);
        const capture = captures.has(files.record)
          ? captures.get(files.record)
          : savedCapture(files);
        captures.set(files.record, capture);
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
          acts: [],
          continues: [],
          pastLastRound: [],
          unresolved: [],
        };
        for (const run of runs) {
          const sorted = sortRun(run, arm, copies, rule, `case ${c.id} ${stage} ${name}`);
          if (sorted.kind === "noReply") audit.noReply += 1;
          else if (sorted.kind === "unrecorded") audit.unrecorded += 1;
          else if (sorted.kind === "ends") audit.ends += 1;
          else if (sorted.kind === "skillRound") audit.skillRound += 1;
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
                ...partTools(sorted.others, part),
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
    const copies =
      audit.copiesFrom === "capture"
        ? "copies from the saved capture"
        : "copies from the case's profile, its capture not saved";
    say(
      b,
      `case ${audit.caseId} ${placeWords(audit)} ${audit.arm}: ${audit.runs} runs, ${audit.judged} judged; judged at a mixed reply with an act ${auditedWords(audit.acts)}; continued now ${auditedWords(audit.continues)}; no verdict past the last round ${auditedWords(audit.pastLastRound)}; unresolved ${auditedWords(audit.unresolved)}; only skill reads ${audit.skillRound}; no mixed reply ${audit.ends}; no reply ${audit.noReply}${unrecorded}; ${copies}`
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
 *  id or no answer, is left out. The file is read once, so its SHA-256 is of the bytes parsed. */
function readSavedLog(file: string): SavedLog | undefined {
  if (!existsSync(file)) return undefined;
  const bytes = readFileSync(file);
  const byUser = new Map<string, Set<string>>();
  for (const { value } of jsonTextLines(bytes.toString("utf8"))) {
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
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  return { file, sha256, byUser };
}

/** The request log of the saved `--out` `from` as continue-mixed and its probe read it: its file
 *  (`logFile`, `<from>/<REQUEST_LOG>`), and `log()`, which reads it (`readSavedLog`) on its first
 *  call and gives that read on every call after, so the file is read at most once. */
function savedLogReader(from: string): Pick<ContinueOptions, "log" | "logFile"> {
  const logFile = path.join(from, REQUEST_LOG);
  let read: SavedLog | undefined | null = null;
  return {
    log: () => {
      if (read === null) read = readSavedLog(logFile);
      return read;
    },
    logFile,
  };
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
 *  saved capture (`savedCapture`), whose installed copies are made again where the cache lacks
 *  them (capture.ts's `restoreCopies`, which refuses a capture whose record names other copies
 *  than the profile now gives), or, where the capture is not saved, the copies of the case's
 *  profile (`profileInstalledCopies`); or why the case cannot be continued, a profile copy the
 *  cache does not hold or cannot read. `captures` holds each capture of the case read already,
 *  by its record file (one per arm and text, `armCaptureDir`, which the case's stages share):
 *  each is parsed, and its copies made again, once per case. */
async function savedArms(
  r: Runner,
  c: Case,
  stage: Stage,
  state: StageResult,
  from: string,
  scratch: string,
  captures: Map<string, Capture | undefined>
): Promise<Map<ArmName, SavedArm> | { readonly unreadable: string }> {
  const dirs = stageArmDirs(r.dirs, state);
  const arms = new Map<ArmName, SavedArm>();
  for (const name of STAGE_ARMS[stage]) {
    const arm = buildArm(c, name, dirs, scratch);
    const files = stageCaptureFiles(from, c.id, arm, state, r.texts);
    const seen = captures.has(files.record);
    const capture = seen ? captures.get(files.record) : savedCapture(files, arm);
    captures.set(files.record, capture);
    if (capture !== undefined) {
      if (!seen) {
        await restoreCopies(c, arm, capture, {
          source: r.source,
          cacheDir: r.cacheDir,
          scratchDir: path.join(r.scratch, "captures"),
        });
      }
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
 *  continues is continued when its record holds every part its next request needs (its reply comes
 *  first in its run, `skillRounds` 0, since a saved step drops earlier rounds' thinking and text;
 *  a session id; the arm's saved capture; and the deployment that answered it, `deployment()`),
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
  const files = readsWords(sorted.reads.map(({ read }) => read));
  if (sorted.kind === "unresolved") {
    const what = [
      ...partTools(sorted.others, "undecided"),
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
  const found = run.skillRounds === 0 && run.userId !== null ? deployment() : undefined;
  const lacks = [
    ...(run.skillRounds === 0
      ? []
      : [
          `its saved reply comes after ${run.skillRounds} answered rounds, whose thinking and text its saved step drops`,
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
 *  verdict, and the runner that made it (`runRunner`). */
function savedRunNote(run: Run, state: StageResult): ContinueMixedRun["saved"] {
  return {
    run: run.run,
    id: run.id,
    round: run.round,
    verdict: run.verdict,
    runner: runRunner(run, state),
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
  if (!sameTexts(r, state)) {
    say(
      r,
      `${where}: left as saved: run with the texts old ${state.texts.old} and new ${state.texts.new}, not --old ${r.texts.old} and --new ${r.texts.new}`
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
    const sorted = sortRun(run, saved.arm, saved.copies, rule, `${where} ${run.arm}`);
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
  const spentBefore = r.gateway.spend().usd;
  const replaysStarted = performance.now();
  const sent = order.map(({ at, deployment, sendPlace }) => {
    const run = state.runs[at] as Run;
    const { arm, capture, copies } = arms.get(run.arm) as SavedArm;
    if (capture === undefined) throw new Error(`${where}: run ${run.run} has no capture`);
    const calls: ContinuationCalls = { requestIds: [], deployments: [], spent: [] };
    const replayed = resumeReplay(
      capture.body,
      { userId: run.userId, routingId: run.routingId ?? null, content: run.reply ?? [] },
      arm,
      pinnedTo(o.continuations, deployment.id, calls),
      {
        headers: capture.record.requestHeaders,
        copies,
        mixed: rule,
        ...(o.furtherRounds === undefined ? {} : { furtherRounds: o.furtherRounds }),
      }
    );
    return { at, run, deployment, sendPlace, calls, replayed };
  });
  const outcomes = await Promise.allSettled(sent.map((each) => each.replayed));
  const judgeStarted = performance.now();
  phases.replaysMs += judgeStarted - replaysStarted;
  for (const outcome of outcomes) {
    if (outcome.status === "rejected" && outcome.reason instanceof NotResumable) {
      throw outcome.reason;
    }
  }
  const replies: ArmReply[] = sent.flatMap(({ run }, index) => {
    const outcome = outcomes[index];
    return outcome?.status === "fulfilled"
      ? [{ arm: run.arm, run: run.run, content: outcome.value.step.message.content }]
      : [];
  });
  const judged = replies.length === 0 ? [] : (await judgeBlind(c, replies, r.judge, seed)).judged;
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
    ...(run.deployments === undefined
      ? {}
      : { deployments: [...run.deployments, ...calls.deployments] }),
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
        error instanceof FurtherRoundLimit
          ? error
          : undefined;
      runs[each.at] =
        stopped === undefined
          ? again(
              each.run,
              error instanceof OtherDeployment
                ? `${CONTINUE_MIXED}: ${error.message}, so it is run again fresh`
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
  const standing = stageOutcome(runs, c.id, stage);
  const toStart =
    standing.state === "running" && Object.values(standing.toStart).some((count) => count > 0);
  const next: StageResult = {
    ...state,
    state: standing.state,
    seeds: order.length === 0 ? state.seeds : [...state.seeds, seed],
    runs,
    usd: state.usd + r.gateway.spend().usd - spentBefore,
    continued: { ...marked, selected: true },
  };
  const dropped =
    standing.state === "unscreened"
      ? (readsPastDrop(runs, stage) ?? refusedDrop(runs, stage))
      : undefined;
  let current = place.put(result, next);
  if (dropped !== undefined) current = { ...current, dropped, droppedAt: stage };
  const file = writeResult(r, current, phases);
  // The runs it changed, in the stage's order, by what it made of each.
  const changed = runs.filter((run, at) => run !== state.runs[at]);
  const of = (kind: "continued" | "waiting" | "run again") => {
    const own = changed.filter((run) => continuedKind(run) === kind);
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
    const stageRunner: Runner = { ...r, dirs: stageArmDirs(r.dirs, next) };
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
  for (const name of STAGE_ARMS[stage]) {
    const saved = arms.get(name);
    if (saved?.capture === undefined) continue;
    const target = capturePaths(armCaptureDir(r.out, saved.arm, state.texts), c.id, name);
    for (const part of ["body", "record"] as const) {
      const inside = path.resolve(saved.files[part]).startsWith(`${path.resolve(from)}${path.sep}`);
      if (existsSync(target[part]) || !inside) continue;
      mkdirSync(path.dirname(target[part]), { recursive: true });
      copyFileSync(saved.files[part], target[part]);
    }
  }
}

/** Whether stage `state` was run with the texts of `r` (this command's `--old` and `--new`), so
 *  its arms can be built again: a stage run with others is left as saved. */
function sameTexts(
  r: { readonly texts: { readonly old: string; readonly new: string } },
  state: StageResult
): boolean {
  return state.texts.old === r.texts.old && state.texts.new === r.texts.new;
}

/** Whether `--only` (`o.only`) selects case `c`'s stage at `place`: every stage without it. */
function selectedBy(o: Pick<ContinueOptions, "only">, c: Case, place: StagePlace): boolean {
  return o.only === undefined || o.only.has(stageKey(c.id, place));
}

/** The stages `--only <file>` selects: each line, blank lines aside, `<case id>`, a tab, and
 *  `screen`, `control` or `confirm <n>` (a case's confirms numbered from 1, oldest first, as
 *  `behaviour audit-mixed` numbers them), each as `<case id>\t<stage>`. Refuses a line of another
 *  form, naming the file and the line. */
function onlyStages(file: string): Set<string> {
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
 * (`refuseOtherRunners`), and an `--only` (`onlyFile`, `onlyStages`) that names a case the file
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
  const o: ContinueOptions = {
    from,
    continuations: loggedGateway(r.gateway, logFile, "continuation"),
    ...savedLogReader(from),
    only: onlyFile === undefined ? undefined : onlyStages(onlyFile),
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
  // Each case's result slim (`forScore`), which holds the stages, the drop and the runners this
  // checks, so no case's replies are held while it runs; the loop below reads each case again.
  const checked = r.cases.flatMap((c) => {
    const each = input(c);
    if (each === undefined) return [];
    const result = forScore(each.result);
    const places = stagePlaces(result);
    for (const place of places) held.add(stageKey(c.id, place));
    if (caseDrop(c, result) !== undefined) return [];
    return places.flatMap((place) => {
      const state = place.get(result);
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
    `${r.cases.length} cases of the skill ${r.cases[0]?.skill} in ${r.casesFile}; the saved runs in ${from} the mixed-reply rule continues are continued into ${r.out}, ${o.only === undefined ? "every stage selected" : `the ${counted(o.only.size, "stage")} --only ${onlyFile} names selected`}, ${furtherRounds === undefined ? "each continuation as far as the live rule goes" : `each continuation at most ${counted(furtherRounds, "further round")}`}; old text ${r.texts.old}, new text ${r.texts.new}`
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
    const scratch = armsScratch(r.scratch, c.id);
    const captures = new Map<string, Capture | undefined>();
    let unreadable: string | undefined;
    for (const place of places) {
      const state = place.get(result);
      if (state === undefined || state.runs.length === 0) continue;
      if (!sameTexts(r, state) || !selectedBy(o, c, place)) continue;
      const armed = await savedArms(r, c, place.stage, state, from, scratch, captures);
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
  const arms: readonly string[] = ScoreArm.options;
  if (armAt <= 0 || !arms.includes(armName) || !Number.isInteger(number) || number < 1) {
    throw new Refused(
      `--probe ${probe} is not <case id>/<arm>/<run number>, an arm of ${arms.join(", ")} and a run number from 1`
    );
  }
  const c = r.cases.find((each) => each.id === caseId);
  if (c === undefined)
    throw new Refused(`--probe ${probe}: ${r.casesFile} holds no case ${caseId}`);
  const result = readResult({ ...r, out: from }, caseId);
  if (result === undefined) {
    throw new Refused(`--probe ${probe}: ${from} holds no result of case ${caseId}`);
  }
  const { log, logFile: savedLogFile } = savedLogReader(from);
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
  const captures = new Map<string, Capture | undefined>();
  for (const place of stagePlaces(result)) {
    const state = place.get(result);
    const run = state?.runs.find((each) => each.arm === armName && each.run === number);
    if (state === undefined || run === undefined) continue;
    const words = placeWords(place);
    if (!sameTexts(r, state)) {
      notes.push(`${words}: its stage was run with other texts than --old and --new`);
      continue;
    }
    const armed = await savedArms(r, c, place.stage, state, from, scratch, captures);
    if (!(armed instanceof Map)) {
      notes.push(`${words}: ${armed.unreadable}`);
      continue;
    }
    const saved = armed.get(run.arm) as SavedArm;
    const sorted = sortRun(run, saved.arm, saved.copies, rule, `case ${c.id} ${words} ${run.arm}`);
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
    await resumeReplay(
      capture.body,
      { userId: run.userId, routingId: run.routingId ?? null, content: run.reply ?? [] },
      saved.arm,
      watched,
      {
        headers: capture.record.requestHeaders,
        copies: saved.copies,
        mixed: rule,
        endsAt: () => true,
      }
    );
  } catch (error) {
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
  "usage: cli.ts behaviour <screen|confirm|control|report|audit-mixed|continue-mixed> --config <skill-cuts.json> --key-file <file> --out <dir> --old <dir> --new <dir> --cases <file>",
  "  [--planted-line <text>] [--budget-usd <n>] [--concurrency <n>] [--after-case <command>] [--request-log <file>]",
  "  [--models <models.yml>] [--omp <binary>] [--repos <dir>] [--hint-model <model>] [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>]",
  "  [--reading-models <file>] (the skill's <cut id>/models/<skill>.json, written by attribute)",
  "  [--again <case id>]... (confirm only: one more confirm of that flagged case)",
  "  [--confirm-flagged] (confirm only: confirm each flagged case even where the skill can get no verdict; report then gives case results only, but for --merged)",
  `  [--control-informative] (control only: run the control case's control even where the skill has fewer than ${MIN_INFORMATIVE_CASES} informative cases; report then gives case results only)`,
  "  [--allow-runners <digest>[,<digest>...]] (take saved stages of these runners as this runner's; report: report results of these runners together)",
  "  [--store-only <file>] (report only: the skill's <cut id>/store-only/<skill>.json, written by attribute)",
  "  [--merged] (report only: the results are a merged set of all the skill's cases, each case's newest run; where its only case-result-only stages are confirms --confirm-flagged made, a loss or no-loss-detected verdict the registered rule reaches stands, its line ending with how many confirms --confirm-flagged made)",
  "  [--route-id <arm>=<uuid>]... (screen, confirm, control: a diagnostic: every run of that arm, in every round, goes with that routing id in place of its round's)",
  "  report needs only --config, --out, --old and --cases; its --request-log is the log it sets the spend against, <out>/requests.jsonl by default",
  `  audit-mixed needs only --config, --out, --old, --new and --cases, and reads --cache-dir and --scratch; it makes no model call and writes <out>/${AUDIT_FILE}`,
  "  continue-mixed --from <saved out> (required) continues the saved runs of --from the mixed-reply rule continues into a new --out, which it never makes --from:",
  "    [--only <file>] (each line <case id>, a tab, and screen, control or confirm <n>: only those stages are continued; the others are kept as saved)",
  "    [--further-rounds <n>] (a continuation sends at most n requests; a reply it would continue again then waits with no verdict, further-round-limit)",
  `    [--probe <case id>/<arm>/<run number>] (send only that run's next request, with no judge, and write <out>/${PROBE_FILE}; not with --only or --further-rounds)`,
].join("\n");

/** The routing id each arm `--route-id <arm>=<uuid>` names is pinned to in `behaviour
 *  <command>`, from each value given: a diagnostic, in which every run of that arm, in every
 *  round, goes with that id in place of its round's (runStage). Refuses a value that is not
 *  `<arm>=<id>`, an arm the command does not run, an id that is not a UUID (the gateway's
 *  ROUTING_HEADER takes a UUID), an arm pinned twice, and any pin given to `report`, which makes
 *  no model call. */
function routePinsOf(given: readonly string[], command: string): Map<ArmName, string> {
  const pins = new Map<ArmName, string>();
  if (given.length === 0) return pins;
  const stage = Stage.safeParse(command);
  if (!stage.success) {
    throw new Refused(
      `--route-id goes with behaviour screen, confirm and control only: ${command === "continue-mixed" ? "continue-mixed sends each run with the routing id it was saved with" : `${command} makes no model call`}. ${BEHAVIOUR_USAGE}`
    );
  }
  const arms: readonly ArmName[] = STAGE_ARMS[stage.data];
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

/** The reading sessions by model `file` holds: attribute.ts's `<cut id>/models/<skill>.json` of a
 *  skill (ModelCountFile). Throws a refusal naming the file. */
function readReadingModels(file: string): ModelCount {
  const parsed = ModelCountFile.safeParse(readJson(file, "the reading sessions by model"));
  if (!parsed.success) {
    throw new Refused(
      `--reading-models ${file} is not a skill's reading sessions by model (attribute's <cut id>/models/<skill>.json): ${schemaFaults(parsed.error)}`
    );
  }
  return parsed.data;
}

/** The options `behaviour <command>` needs (`required`): `report` reads the saved results alone;
 *  `audit-mixed` builds the arms from `--new` too; a stage command calls the model
 *  (`--key-file`), and `continue-mixed` does too, from a saved `--out` (`--from`). */
function behaviourNeeds(
  report: boolean,
  audit: boolean,
  continuing: boolean
): readonly ("config" | "key-file" | "out" | "from" | "old" | "new" | "cases")[] {
  if (report) return ["config", "out", "old", "cases"];
  if (audit) return ["config", "out", "old", "new", "cases"];
  if (continuing) return ["config", "key-file", "out", "from", "old", "new", "cases"];
  return ["config", "key-file", "out", "old", "new", "cases"];
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
      cases: { type: "string" },
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
      again: { type: "string", multiple: true },
      "confirm-flagged": { type: "boolean" },
      "control-informative": { type: "boolean" },
      "allow-runners": { type: "string" },
      "store-only": { type: "string" },
      merged: { type: "boolean" },
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
  if (values["control-informative"] !== undefined && command !== "control") {
    throw new Refused(
      `--control-informative goes with behaviour control only: report reads from the case results whether their controls were made with it. ${BEHAVIOUR_USAGE}`
    );
  }
  const storeOnlyFile = values["store-only"];
  if (storeOnlyFile !== undefined && command !== "report") {
    throw new Refused(
      `--store-only goes with behaviour report only: no stage command reads it. ${BEHAVIOUR_USAGE}`
    );
  }
  if (values.merged !== undefined && command !== "report") {
    throw new Refused(
      `--merged goes with behaviour report only: it says how report scores the results it reads. ${BEHAVIOUR_USAGE}`
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
  const routePins = routePinsOf(values["route-id"] ?? [], command ?? "");
  const report = command === "report";
  const audit = command === "audit-mixed";
  const given = required(values, behaviourNeeds(report, audit, continuing), BEHAVIOUR_USAGE);
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
  const readingModels = values["reading-models"];
  const behaviour: Behaviour = {
    io,
    command: `behaviour ${command}`,
    out: given.out,
    casesFile: given.cases,
    cases,
    servedModels: config.servedModels,
    readingModels:
      readingModels === undefined
        ? undefined
        : { file: readingModels, count: readReadingModels(readingModels) },
    allowRunners,
    storeOnly: storeOnlyFile === undefined ? undefined : readStoreOnly(storeOnlyFile),
    merged: values.merged === true,
  };
  const log = values["request-log"] ?? path.join(given.out, REQUEST_LOG);
  if (report) {
    reportCommand(behaviour, log);
    return EXIT.ok;
  }
  if (audit) {
    mustBeDirectory("--new", given.new);
    auditMixedCommand(
      behaviour,
      { old: given.old, new: given.new },
      values["cache-dir"] ?? path.join(tmpdir(), CACHE_DIR),
      values.scratch ?? path.join(tmpdir(), SCRATCH_DIR)
    );
    return EXIT.ok;
  }
  if (command === "screen" && behaviour.readingModels !== undefined) {
    const refusal = servedScreeningRefusal(behaviour.readingModels.count);
    if (refusal !== undefined) {
      throw new Refused(
        `the skill ${cases[0]?.skill} gets no verdict, and no case of it is screened: ${refusal.reason} (--reading-models ${readingModels}); the walk of the cut's ranking goes on to its next skill`
      );
    }
  }

  mustBeDirectory("--new", given.new);
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
  const scratch = values.scratch ?? path.join(tmpdir(), SCRATCH_DIR);
  const cacheDir = values["cache-dir"] ?? path.join(tmpdir(), CACHE_DIR);
  const plantedLine = values["planted-line"];
  const source = values.repos === undefined ? ghSource() : localSource(values.repos);
  const hints = gatewayHints(loggedGateway(gateway, log, "hint"));
  const texts = { old: textDigest(given.old), new: textDigest(given.new) };
  const runner: Runner = {
    ...behaviour,
    dirs: {
      old: given.old,
      new: given.new,
      ...(plantedLine === undefined ? {} : { plantedLine }),
    },
    texts,
    scratch,
    cacheDir,
    gateway,
    replays: loggedGateway(gateway, log, "replay"),
    judge: loggedGateway(gateway, log, "judge"),
    source,
    capture: async (c, arm) => {
      const outDir = armCaptureDir(given.out, arm, texts);
      const scratchDir = path.join(scratch, "captures");
      const saved = savedCapture(capturePaths(outDir, c.id, arm.name), arm);
      if (saved !== undefined) {
        await restoreCopies(c, arm, saved, { source, cacheDir, scratchDir });
        return saved;
      }
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
        scratchDir,
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
    controlInformative: values["control-informative"] === true,
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
  mustBeDirectory("--skill", given.skill);
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
    memoryLimitBytes: MEMORY_LIMIT_BYTES,
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
  mustBeDirectory("--skill", given.skill);
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
  "  behaviour <screen|confirm|control|report|audit-mixed|continue-mixed> --config <skill-cuts.json> --key-file <file> --out <dir> --old <dir> --new <dir> --cases <file> ...",
  "  read --prompt <name> --in <input.json> --out <answer.json> ...",
  "  control <map|attack|same-job|sentence> --in <input.json> --out <result.json> ...",
  "  count-sql --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>] [--query count|models]",
  "  count-sql --config <skill-cuts.json> --store <db> --query roots|hashes",
  "  count-sql --config <skill-cuts.json> --query transcript --day <YYYY-MM-DD> [--hour <0-23>] [--run]",
  "  count-sql --config <skill-cuts.json> --query transcript-coverage [--run]",
  "  load --config <skill-cuts.json> --key-file <file> --skill <dir> [--name <name>] --tasks <file> --host <omp|claude> [--profile <file>] [--test-setup <owner/repo>@<commit>:<path>] --out <dir> ...",
  "  load audit-mixed --out <dir> --skill <dir> [--cache-dir <dir>]",
  "  copies --config <skill-cuts.json> --at <ISO instant> --out <dir> [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]",
  "  attribute --config <skill-cuts.json> --count <dir> --copies <dir> --spans <file> --out <dir> [--changed <cut id>=<file>]... [--models <dir>]",
  "  build-cases --config <skill-cuts.json> --key-file <file> --cut <id> --skill <name> --old <dir> [--new <dir>] --sessions <file> --seed <n> --copies <dir> --count <dir> --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>] [--store-only <file>]",
  "  build-cases --config <skill-cuts.json> --old <dir> --copies <dir> --count <dir> --reprofile <cases.jsonl> --out <new cases.jsonl>",
  "  build-tasks --config <skill-cuts.json> --key-file <file> --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir> --context-session <store>:<hash> [--test-setup <owner/repo>@<commit>:<path>]] [--max-candidates <n>] [--max-search-minutes <m>]",
  "  build-tasks --config <skill-cuts.json> --copies <dir> --count <dir> --reprofile <tasks.jsonl> --profiles-out <new dir> [--context-session <store>:<hash>]",
  "  build-tasks --first-read --config <skill-cuts.json> --key-file <file> --skill <dir> --sessions <sessions.jsonl> --seed <n> [--max <n>] --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir>]",
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
      case "build-cases":
      case "build-tasks":
        return await buildMain(argv, {
          out: io.out,
          err: io.err,
          gh: spawnGh,
          now: () => performance.now(),
          spendRecorder: io.spendRecorder,
        });
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
