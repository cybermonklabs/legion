/**
 * The builders of the runner's inputs (the behaviour test's candidate sessions, their cases and
 * each case's context, and the load test's tasks): `build-cases` turns real sessions into the
 * case file the `behaviour` commands read (cases.ts), and `build-tasks` writes the tasks file the
 * load test reads (load.ts).
 * `main(argv, io)` runs either; `argv` starts with the command's name, so cli.ts can pass its own
 * whole. A store is read only with the configuration's `psqlCommand` (config.ts), one query a
 * call, each a SELECT of this runner's SQL (countsql.ts); every model call is a reader of
 * readers.ts through gateway.ts, signed in by `--key-file`.
 *
 *   bun scripts/skill-cuts/build.ts build-cases --config <skill-cuts.json> --key-file <file>
 *       --cut <id> --skill <name> --old <skill dir at the cut's base> --sessions <file>
 *       --seed <n> --copies <copies out dir> --count <count dir> --out <cases.jsonl> [--max <n>]
 *       [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>]
 *       [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]
 *   bun scripts/skill-cuts/build.ts build-cases --config <skill-cuts.json> --old <skill dir at
 *       the cut's base> --copies <copies out dir> --count <count dir> --reprofile <cases.jsonl>
 *       --out <new cases.jsonl>
 *   bun scripts/skill-cuts/build.ts build-tasks --config <skill-cuts.json> --key-file <file>
 *       --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir>
 *       --copies <copies out dir> --count <count dir> --context-session <store>:<hash>]
 *       [--max-candidates <n>] [--max-search-minutes <m>]
 *   bun scripts/skill-cuts/build.ts build-tasks --first-read --config <skill-cuts.json>
 *       --key-file <file> --skill <dir> --sessions <sessions.jsonl> --seed <n> [--max <n>]
 *       --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <copies out dir>
 *       --count <count dir>]
 *
 * **build-cases.** `--sessions` holds the candidate sessions, one JSON line each:
 * `{"store", "hash", "path"?}`, `hash` the file hash count.sql names a file by (16 hex characters
 * of md5 of its path). A line without `path` is resolved with sessions.sql, one query per store.
 * The candidates are taken in the order judge.ts's `blindOrder` gives them under `--seed` (the
 * summary records the seed and the order), one case per session, until `--max` cases are built,
 * or the readers' dollars at list price reach `--budget-usd`, when no further reader call starts
 * and the build stops (exit 3) with what it has.
 *
 * **The stop rule** (`stopBy`): before the build reads a further session, after each session
 * decided, it stops (exit 4) as soon as
 * `--stop-zero-case-usd` (STOP_ZERO_CASE_USD unless given) is spent with no case built; or, once
 * at least `--stop-after-sessions` (STOP_AFTER_SESSIONS) are decided, when the cost projected to
 * `--max` cases from its own cases and spend so far (max × spent ÷ cases, unbounded with no case)
 * passes `--stop-projected-usd` (STOP_PROJECTED_USD). The summary's `stopped` then holds `{rule,
 * sessions, cases, spentUsd, projectedUsd}` (`projectedUsd` null when unbounded); the sessions
 * still open are recorded `not-needed`, and every drop keeps its reason. A run that goes on
 * checks the rule after the sessions the earlier runs decided, before it reads any, so a build
 * the rule stopped stops again at once with the same bounds. The projection needs `--max`.
 *
 * Up to `--concurrency` sessions (BUILD_CONCURRENCY unless given) are open at once: pulled (one
 * store query at a time across them) and read, the gateway taking as many calls at once. A
 * session's outcome is decided in the seeded order, a session that finished waiting for those
 * before it, so the cases kept are the first `--max` cases of that order, as one session at a time
 * would keep them. Once the build ends (at `--max`, at the budget, or by the stop rule), each
 * session still open is stopped before its next reader call and recorded with what it spent, as
 * `not-needed` (or as stopped by the budget). With more than one session open, a session before
 * the one that reached the budget can be stopped by it too.
 *
 * **Going on.** The summary records the run's inputs: the cut, the skill, the seed, and the
 * SHA-256 of `--old`'s files, the sessions file, the copies files read and the count's query
 * outputs. When `--out` and its summary are there from a run of the same inputs, the build goes on
 * from them: every session they decided (dropped, or a case the cases file holds) is kept as it
 * was, with no pull and no reader call, and the build goes on in the same order from the first
 * session not decided, until `--max` cases in all; the budget counts the dollars already spent.
 * Inputs that differ are refused (exit 2), naming each; so is a cases file with no summary, and a
 * summary with no inputs. Each case is appended to the cases file as one whole line and then the
 * summary is written to a temporary name and renamed, so both stay readable after a kill at any
 * moment; a case line the summary does not record (a kill between the two) is left out when the
 * build goes on, and its session is read again. For each session:
 *
 * 1. Its transcript is pulled by path with pull.sql, PULL_PAGE slices of at most 1 MiB a call,
 *    paging by place, into a file of its own (`<out>.spool/`, removed once the session is
 *    decided), each slice written as its row comes, read by psql-rows.ts's `PsqlTable` and
 *    checked by its `PullCheck`; so one row is held at a time, however long the file. It is read
 *    from there line by line (`readTranscript`), each line parsed as transcript.ts's entry; the
 *    text after the last newline, a line still being written, is left out when it does not
 *    parse. Only the entries the builder reads whole are held whole (`heldWhole`: the header, a
 *    `session_init`, model and thinking changes, context resets, a system prompt's digest, Envoy
 *    role claims); every other entry is held light, with where its line is, a message with its
 *    roles, tool calls and `[Skill file: …]` line, and is read whole again only where a check or a
 *    reader needs it (the reader's windows, the reads it offers, the case's messages, the
 *    compaction before the step). So a session holds its entries' light forms and what its case
 *    and windows need, not its file.
 * 2. The session's messages are those of its current branch (the parent chain of its last entry),
 *    in order, as Oh My Pi 18.18.6 gives them to the model: each `message` (less an assistant
 *    message it leaves out: one marked `retryRecovery`, or an error with no content), each
 *    `custom_message` as a `custom` message (less the two plan-mode kinds it leaves out), each
 *    `branch_summary` as a `branchSummary` message. A string `content` is made one text block.
 * 3. The first successful read of a file of the skill (`read` of a `skill://<skill>` URL, as
 *    cases.ts's `skillReadFile` reads one) is found, and the copy its `[Skill file: <path>]` line
 *    names is placed by attribute.ts's own rules (`placer`: the prefix table, then a checkout,
 *    then the store's context), from `--count` and `--copies`, the two attribute.ts made the
 *    sessions file from. The session is kept only when they place the read at a copy of the cut's
 *    repository; one they place at another copy, or at none, is dropped with the rule named.
 *
 *    Every check that needs no model's answer runs here, before the session's first reader call,
 *    so a session such a check drops costs no reader spend: the placement; the model and thinking
 *    level, when none the session sets from the read on can give its step a model, a level Oh My
 *    Pi takes, or a model the gateway serves (each the drop the step's own check makes); the
 *    session's role or subagent context; the lines a case may name (step 4); and the profile at
 *    the session's start (below), built from every read the reader can reach: when no unit of
 *    those reads ships the cut repository's copy of the skill, no step's profile installs it. The
 *    checks of the step and its line (its model, the compaction and by-path reads before it, the
 *    line in the base text and in the read, a quote or restatement of it, and the profile of the
 *    reads before it) need the reader's answer, so they run after it and before the restatement
 *    check's and the audit's readers.
 * 4. The case reader (`skill-cuts read --prompt case`) is offered only the lines of the skill's
 *    base
 *    text (`--old`) that a case may name: those of each file that the session's own successful
 *    reads of that file hold whole (the text after its `[Skill file: …]` line, each line trimmed,
 *    among the messages the reader reads), each with its number in the base file. A file the
 *    session did not read is not offered, and a session whose reads hold no line of the base text
 *    is dropped before any reader call (`no-common-line`), since the skill's text may have changed
 *    after the session read it. It reads them and the session's messages window by window, one
 *    call a window, from the read's call on: each window at most READER_MESSAGES messages and
 *    READER_CHARS characters, and before it, nearest first, at most READER_EARLIER_MESSAGES
 *    messages and READER_EARLIER_CHARS characters of those after the last compaction or context
 *    reset before the window. The next window starts after the last message of the one before,
 *    until the reader proposes a step, READER_WINDOWS windows are read (at most READER_WINDOWS ×
 *    READER_MESSAGES messages after the read), or the messages end. No window reads past the
 *    first compaction or context reset after the skill's read: a step after it would follow a
 *    compaction that follows the read, which drops it, so a session whose windows reach it with no
 *    step is dropped for that (`read-before-compaction`). Each message is cut to MESSAGE_CHARS
 *    (its start and its end kept), and each read result of the skill's files is left out with a
 *    note. The reader proposes the first step the skill governs, its message index, the line it
 *    exercises (one of those offered), the criterion, a violating step (a plausible next
 *    step that breaks the line and otherwise looks valid), and each message before the step that
 *    states the line's instruction in any wording; or no step. The session's record in the summary
 *    says how many windows the reader read (`windows`).
 * 5. The case: `messages` the model's messages before the step, `realStep` the real assistant
 *    message at it, `violatingStep` the reader's violating step as an assistant message (its text,
 *    then each tool call), `skillReads` the index of each read result of a `skill://<skill>` file
 *    in `messages`, `model` and `thinking` the branch's last `model_change` and
 *    `thinking_level_change` before the step, `sessionDigest` its last `system-prompt-digest`
 *    before the step, `sessionStart` the header's timestamp, `source` the store and the SHA-256 of
 *    the path (never the path), `context` (`devbox`; `legion:<project>:<role>` from the session's
 *    role, below; or `subagent:<agent>`), and `profile` (below). Where a compaction precedes the
 *    step and every read of the skill follows it, `messages` is what Oh My Pi gives the model
 *    after it: the summary as a `compactionSummary` message, the entries it kept, and those after
 *    it.
 * 6. Secrets are taken out of the case (`removeSecrets`: private keys, bearer tokens, known token
 *    shapes, credentials in URLs, and key, token, secret and password assignments), each counted
 *    in `secretsRemoved`.
 * 7. The restatement check (the `restate` reader) reads every message before the step that the
 *    case keeps, back to its first, less the skill's own read results: each rendered whole
 *    (judge.ts's `renderMessage`, then its thinking and its summary, if any), a message longer than
 *    RESTATE_PART_CHARS in parts that overlap by RESTATE_OVERLAP characters, in windows of at most
 *    RESTATE_CHARS characters, one call a window, at most RESTATE_WINDOWS windows. It lists each
 *    message that states the line's instruction in any wording, with words quoted from it, which
 *    its schema checks against the message. A restatement in any window drops the case. A case
 *    whose messages need more windows, or one of whose windows the reader could not read, is
 *    dropped as not checked whole, never kept. The session's record says how many messages the
 *    check must read (`before`), how many it read whole (`read`), and in how many windows: a kept
 *    case has `read` equal to `before`. The case reader's own list of restatements (step 4), from
 *    the messages it read, drops a case first.
 * 8. The audit (the `audit` reader) reads the line, the messages before the step as the judge reads
 *    them (judge.ts, `beforeText`) and the real step (`stepText`), and never the criterion, which
 *    was written looking at the real step; it answers `follows`, `breaks` or `not-governed`, with a
 *    reason. A case is kept only when the real step follows the line; the audit's answer and
 *    reason go in the case's `evidence` and the session's record.
 *
 * A session gives no case, and its drop is counted by reason (DROP_REASONS), when: the store holds
 * no file of its hash; the pull fails or returns nothing; a line is not an entry; it has no
 * header; no read of a skill file succeeded; the first read is not the cut repository's copy; a
 * subagent's `session_init` line is not whole; no form below names a Legion session's role; the
 * session's reads of the skill hold no whole line of the base text; the reader's every reply is
 * refused, its call fails, or it finds no step in the windows it reads (or a step at or before
 * the read); the line is not a whole line of the base text, or no successful read result of its
 * file before the step holds it; a skill read precedes a compaction (or a context reset) that
 * precedes the step, or the compaction is of a kind this file does not rebuild (one that keeps
 * data for the provider, or keeps no entry of the branch); an assistant message
 * before the step quotes the line (less a leading list, heading or quote mark); the case reader or
 * the restatement check finds a message before the step that states the line's instruction in
 * other words, or the check cannot read them all; the audit finds the real step breaks the line,
 * or the line does not govern it, or gives no answer; no route of the gateway serves the model
 * before the step, or there is none; a read before the step reads a file of the skill by its path
 * or with a selector, which arms.ts does not rebuild (a path that ends `/<skill>/SKILL.md`, holds
 * `skills/<skill>/`, or is under the skill's installed directory); a GitHub read the profile needs
 * fails; or the profile's units install no copy of the skill from the cut repository. A session
 * the readers reach costs one case reader call a window, one restatement call a window, and one
 * audit call.
 *
 * **The model.** A case runs on the session's own model, as its last `model_change` before the
 * step names it (`<gateway>/claude-opus-5-5`, `openai/gpt-6.1-sol`), and on no other. The
 * gateway serves a model when some route does: its id (the text after the last `/`, a thinking
 * suffix left out) is the id of one of the configuration's `servedModels`, the read count's rule
 * (attribute.ts). A session on a model no route serves is dropped (`model-not-served`), before any
 * reader reads it when none of its models from the skill's read on is served.
 *
 * **A subagent's session** (a transcript under its parent's, `<parent less .jsonl>/<Name>.jsonl`)
 * starts with a `session_init` line that records the system prompt it was given (its blocks, the
 * skill listing among them), its tool names and its agent. Its context is `subagent:<agent>`, and
 * its profile carries that prompt and those tools, which the capture sends in place of a rebuilt
 * prompt (capture.ts, "A recorded prompt"); it holds no context files apart, since the recorded
 * prompt holds them. The case's `evidence` says the prompt is the recorded one.
 *
 * **The role** of a top-level session of a Legion store (`legion:<project>`) is read, in this
 * order, from:
 * 1. its first user message: the daemon's phase task, `Continue <title>. Issue: <key>. Phase:
 *    <phase>.` (workflow/effects.go), whose phase is worked by phase.go's `RoleFor` (PHASE_ROLES);
 *    the start of a tree's root architect, `<KEY> is yours and …` (workflow/architect_start.go),
 *    `architect-root`; or the controller's start, `Legion controller start: …`
 *    (daemon/controller.go), `controller`;
 * 2. else the daemon's catch-up notice to an architect (an Envoy message `from: agent`,
 *    workflow/catch_up.go): `the tree of <KEY> …` for a tree's root (`architect-root`), `the issues
 *    under <KEY>, which you own …` for an architect of a child issue (`architect`);
 * 3. else the Envoy role the session claims (its `envoy-role-claim` entry), the daemon's claim
 *    token `legion-<project>-<issue>-<role>` (claim/token.go, NewToken), whose issue must be the
 *    one the session's path names as its workspace (Oh My Pi keeps a session under a directory
 *    named after its working directory, `--legion-workspaces-<owner>-<name>-<issue>--`), or the
 *    controller's token `legion-<project>-controller`. An architect's claim alone does not say
 *    whether its issue is its tree's root, which decides its role prompt
 *    (`Compose(architect, isRoot)`), so it gives no role.
 * A session no form places is dropped (`no-role`), with its first words (at most 80 characters)
 * in the drop's detail. A phase worker that an architect's Envoy message woke (its first user
 * message comes later, or never) is read from its claim. The case's `evidence` says which form
 * gave the role.
 *
 * **The profile**, from the files of the copies output (`copies --out`: prefix-table.tsv,
 * installers.tsv, manifests.tsv, repositories.tsv), the session's own reads and
 * each unit's repositories at the session's start, each part with its evidence in the case:
 * - The units installed: each unit (`via`) of each prefix row that a `[Skill file: <path>]` of a
 *   read result before the step is under; for a path no row maps, each unit of the prefix rows of
 *   the copy attribute.ts's rules place the read at (a checkout, the store's context); and, for a
 *   Legion context, each Oh My Pi unit with a prefix row under `/opt/legion/` (a package the
 *   worker image carries) and each unit an `omp-plugins` manifest installs. A read no rule
 *   places is listed as left out; a copy a read is placed at that no prefix row names a unit for
 *   is installed alone. A read whose skill none of those units ships at the session's start (the
 *   copies output names a unit as the manifests name it later: a marketplace or plugin renamed
 *   since, whose old name the read's plugin cache directory shows) adds each unit of the prefix
 *   rows that the same manifests give at the session's start (copies.ts's rules, below) and that
 *   its path is under, and that unit's copy is the one the session read.
 * - Commits: each repository at the last commit of its default branch (repositories.tsv, else
 *   GitHub's) at or before the session's start. Every GitHub read goes through `gh api`
 *   (io.gh): GET calls, and GraphQL queries that read a repository's files in batches.
 * - Skills: what each unit ships at that commit, not what installers.tsv lists, which is what it
 *   ships at the later commit the copies output read. Each repository of a unit (its manifest's,
 *   and those of the copies installers.tsv lists for it) is read at its commit as copies.ts reads
 *   one: its tree, each `SKILL.md` outside the configuration's `excludePaths`, and each manifest
 *   file by
 *   its path; copies.ts's `resolveInstallers` then gives each unit's skills (the prepack script's
 *   list for a package, a plugin's `skills/`, an installer's linked directory), by name. Two units
 *   that ship copies of one name install the one the session read (its first read of that name,
 *   placed by attribute.ts's rules: the host gave the session that copy) when a unit ships it
 *   then, else the first unit's. So every skill of a profile is in its repository at the commit
 *   the profile names; the evidence names each skill installers.tsv lists that the unit did not
 *   ship then, and each it shipped then that installers.tsv does not list. Plugins: an Oh My Pi
 *   package's directory (the `omp-package` manifest's, at that commit), or a Claude Code plugin's
 *   directory (its skills' parent), named by its last part.
 * - Context files: for a Legion context, the `AGENTS.md` of the repository of the session's
 *   working directory (`/legion/workspaces/<owner>/<name>/…`) when it has one at that commit, and
 *   the project's instructions file named by its project file (a `legion-project` manifest of
 *   manifests.tsv, `instructions:` read at that commit, under `<dir>/instructions/`). A devbox
 *   session's working directory names no repository, so it has none of those; it has the
 *   user-level context file Oh My Pi 18.18.6 reads (discovery/claude.ts): one file at user level,
 *   CLAUDE.md in Claude Code's config directory, which a dev box's CLAUDE_CONFIG_DIR names, the
 *   `.claude/` of the repository that installs its units. That is the one repository of the
 *   profile's units that holds Claude Code's settings there (`.claude/settings.json`, copies.ts's
 *   claude-settings manifest) at the session's start, and its `.claude/CLAUDE.md` at that commit
 *   (context.ts writes it as `CLAUDE.md` in a scratch Claude Code config directory that the
 *   capture names in CLAUDE_CONFIG_DIR, where Oh My Pi reads it as its user-level file; load.ts
 *   gives Claude Code it as user memory). Each one the copies output or the session does not name
 *   is listed as left out: for a devbox, when no repository or more than one of its units holds
 *   those settings, or that one has no CLAUDE.md then. A subagent's case
 *   has none: its recorded prompt holds them. Its units and skills are read as its store's
 *   context's are.
 *
 * Output: the case file `--out` (one JSON line per case, written whole after each case and checked
 * with cases.ts's `loadCases` against `--old` at the end) and `<out>.summary.json`: the seed and
 * the order, the sessions tried, each session's outcome, model and the reader's dollars on it, the
 * cases built, the drops by reason, the sessions tried and the drops of each model (`byModel`), the
 * bounds, the pulls, the gateway's spend, and why the build stopped, if it did.
 *
 * **build-cases --reprofile** builds the profile of each case of a case file again, from the
 * case alone: its store, the `[Skill file: …]` lines of its messages, its start, and whether it
 * carries a recorded prompt. It reads no session and calls no reader. The session's working
 * directory is not in a case; it is taken as the workspace of the repository whose `AGENTS.md`
 * the old profile holds, and a case whose old profile holds none keeps its old evidence of why.
 * The new file's cases differ from the old ones only in `profile` (its skills, plugins and context
 * files; a recorded prompt and its tools stay) and in the profile's part of `evidence`.
 * `<out>.summary.json` lists, per case, the skills added and removed. A case whose profile cannot
 * be built, or no longer installs its skill, is left out and named, and the command exits 1.
 *
 * **build-tasks** (the load test's tasks): the tasks reader reads the skill's name and description
 * (`--skill`'s SKILL.md frontmatter) with no messages and proposes search terms; tasks.sql runs
 * per store and per UTC day of the configuration's window for first user messages holding them;
 * the distinct messages (at most TASK_MESSAGES, spread evenly over the stores' rows in path order)
 * go to the tasks reader again, which picks at least 5 need and 5 near-miss tasks from them, each
 * by its id, and writes the missing ones. It says of each need task whether the skill governs its
 * first step (an agent doing it would read the skill at once, before any other act); a need task
 * whose first step it does not govern is left out, and the summary counts it (`droppedNeed`) and
 * keeps it with the reader's reason (`dropped`). The tasks file holds one JSON line per task,
 * labelled before any run: `{"id", "text", "label", "source": "session", "store", "pathHash"}` or
 * `{"id", "text", "label", "source": "written"}`. `<out>.summary.json` holds the terms, each
 * query's
 * row count (or its error), the messages offered, each task's reason, and the spend. Its queries,
 * one per store and day, take most of its time (an hour and more on the real stores), so it goes
 * on as build-cases does: the summary records the inputs (SKILL.md's SHA-256, the stores and the
 * window), the terms and each query as it ends, and `<out>.found.jsonl` the messages each found
 * (path hashes and texts, appended whole before the summary records the query). A run of the same
 * inputs goes on from them with no terms call and no query already recorded; a finished build is
 * left as it is; other inputs are refused. The found file is removed once the tasks are written.
 *
 * **build-tasks --first-read** (the load test's positive control) writes the control's need
 * tasks from the sessions file of the skill, unchosen: see `buildControlTasks`.
 *
 * **--profiles-out** (both modes): each task runs in the rebuilt context of the session it came
 * from. For each task from a session, the session
 * is pulled again and its profile rebuilt as build-cases rebuilds a case's at the session's start
 * (`sessionProfile`: the context, `caseProfile` from every read of its branch with `--copies` and
 * `--count`, a subagent's recorded system prompt and tools), less the tested skill, and written
 * to `<dir>/<task id>.json` (load.ts's LoadProfile); the task line's `profile` is its path. A
 * written task takes the profile of `--context-session <store>:<hash>` (required in the reader's
 * mode; rebuilt before any reader call, and a session that gives none fails the build). The
 * summary's `profiles` records the context session, each task's profile and its evidence, and why
 * a task has none. A finished build gets none afterwards: profiles come with a fresh `--out`.
 * A task whose context does not list the tested skill tests nothing (`listsSkill`: a subagent's
 * recorded listing lacks its line, or another context's profile does not install it), so with
 * `--profiles-out` such a session is dropped (`skill-not-listed`) and counted: in the reader's
 * mode each offered message's session is rebuilt first and the next candidate taken in its
 * place (`offeredMessages`, `candidateDrops`), in `--first-read` when the session is tried
 * (`skillNotListed`); and a `--context-session` whose context does not list it is refused.
 *
 * **--model <model>** (both modes: a load test runs on one model, and the tasks taken from real
 * sessions come only from sessions on it): a candidate session is kept only when its first request
 * ran on `<model>`, as the session's file names it and attribute.ts's models table writes it
 * (`firstReplyModel`: its last `model_change` before its
 * first assistant message, a thinking suffix left out, so `<gateway>/openai/gpt-6.1-sol` and
 * `openai/gpt-6.1-sol` are two models). Every other session is dropped and counted
 * (`other-model`): in the reader's mode each offered message's session is pulled and the next
 * candidate taken in its place (`candidateDrops`), in `--first-read` when the session is tried,
 * and that summary's `model` says how many sessions tried on the model read the skill in their
 * first reply. Written tasks are unchanged. The summary names the model; the inputs hold it, so a
 * run that goes on with another model is refused. Without `--model` nothing of this happens.
 *
 * **--max-candidates <n>, --max-search-minutes <m>** (the reader's mode, with `--model` or
 * `--profiles-out`, whose candidate search pulls each candidate's whole transcript): the search
 * ends before its next candidate once the candidates decided (accepted or dropped) reach n, or its
 * own time reaches m minutes, both summed over the runs of one `--out`; the time is checked
 * between candidates, so the last one decided can run past it. The reader then gets the messages
 * accepted so far and writes the rest of the tasks, as when the candidates run out. Each decision
 * is appended to `<out>.candidates.jsonl` as it is made, after a first line that holds the bounds,
 * and the bound that ends the search too; a run that goes on takes each candidate decided there as
 * it was decided, with no pull, and refuses (exit 2) other bounds than those, naming both values.
 * A file that holds only the bounds, or none, takes any bounds. The summary's `search` holds the
 * bounds, what ended the search (a bound's flag, `candidates ran out` or `enough offered`), the
 * candidates decided and accepted, and the search's seconds; `candidateDrops` holds the drops by
 * reason. The file is removed once the summary is written. With neither bound the search is as
 * above, and no file is written.
 *
 * Exit status: 0 written; 1 failed; 2 refused before any store read or model call; 3 stopped at
 * `--budget-usd`, with the cases built so far written; 4 stopped by the stop rule, the same.
 */
import { createHash } from "node:crypto";
import {
  appendFileSync,
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  rmSync,
  statSync,
  writeSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { mapLimit, outRefusal } from "../skill-scenarios/production-dispatch";
import {
  readCalls,
  readTargets,
  SessionEntry,
  SessionMessage,
  skillFileHeader,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import {
  type InstallersRow,
  type ManifestsRow,
  modelId,
  type Placer,
  type PrefixTableRow,
  placer,
  type R4Output,
  readCount,
  readR4,
  sessionModel,
  type Via,
  vias,
} from "./attribute";
import { armPromptBlocks } from "./capture";
import {
  Case,
  loadCases,
  type Profile,
  type ProfileSkill,
  pathHash,
  skillReadFile,
  Thinking,
  textLines,
} from "./cases";
import { configFrom, json, parse, Refused, required, schemaFaults, writeWhole } from "./common";
import { type SkillCutsConfig, storeNamed } from "./config";
import {
  BLOB_BATCH,
  BLOB_BATCH_BYTES,
  BlobBatch,
  blobBatches,
  blobQuery,
  CopyIndex,
  frontmatter,
  type Gh,
  GitBlob,
  graphqlArgs,
  keepsEntry,
  type Manifest,
  parseJson,
  parseManifest,
  prefixMatches,
  repoPath,
  repositoryFiles,
  resolveInstallers,
  type SkillCopy,
  skillFields,
  spawnGh,
  type TreeEntry,
} from "./copies";
import { mtimeRange, PULL_PAGE, pullSql, sessionsSql, tasksSql } from "./countsql";
import {
  addModelSpend,
  BudgetSpent,
  createGateway,
  type Gateway,
  type Spend,
  usdOf,
} from "./gateway";
import {
  beforeText,
  blindOrder,
  cut,
  MESSAGE_CHARS,
  readsSkillUrl,
  renderMessage,
  skillReadResults,
  stepText,
} from "./judge";
import { firstMessageLoad } from "./load";
import { PsqlTable, PullCheck } from "./psql-rows";
import {
  AuditAnswer,
  CaseAnswer,
  type CaseInput,
  ReaderFailed,
  RestateAnswer,
  runReader,
  skillFiles,
  TASKS_PER_LABEL,
  TasksAnswer,
  type TasksInput,
} from "./readers";
import { isObject } from "./replay";

/** Exit statuses: `stopped` at `--budget-usd`, `stoppedByCost` by the stop rule (`stopBy`). */
export const EXIT = { ok: 0, failed: 1, refused: 2, stopped: 3, stoppedByCost: 4 } as const;
/** The most sessions build-cases reads at once, unless `--concurrency` says otherwise. */
export const BUILD_CONCURRENCY = 4;
/** The stop rule's bounds, unless `--stop-projected-usd`, `--stop-after-sessions` and
 *  `--stop-zero-case-usd` say otherwise (README.md, "build-cases and build-tasks"). */
export const STOP_PROJECTED_USD = 200;
export const STOP_AFTER_SESSIONS = 40;
export const STOP_ZERO_CASE_USD = 60;

/** The bounds of the stop rule of one build. */
interface StopRule {
  readonly projectedUsd: number;
  readonly afterSessions: number;
  readonly zeroCaseUsd: number;
}

/** Why the stop rule stopped a build, as its summary records it: the rule (its flag's name), the
 *  sessions decided and the cases built then, the readers' dollars spent, and the projected cost
 *  to `--max` cases (null when no case is built, so it is unbounded). */
interface RuleStop {
  readonly rule: "stop-projected-usd" | "stop-zero-case-usd";
  readonly sessions: number;
  readonly cases: number;
  readonly spentUsd: number;
  readonly projectedUsd: number | null;
}

/**
 * The stop rule of a build of at most `max` cases, once `sessions` are decided, `cases` of them
 * cases, and `spentUsd` is spent: it stops as soon as `zeroCaseUsd` is spent with no case; and,
 * once at least `afterSessions` are decided, when the cost projected to `max` cases from the
 * build's own cases and spend so far (max × spent ÷ cases; with no case, unbounded) passes
 * `projectedUsd`. A build with no `--max`, or one that has its `max` cases, is not stopped by the
 * projection.
 */
export function stopBy(
  rule: StopRule,
  state: { sessions: number; cases: number; spentUsd: number },
  max: number
): RuleStop | undefined {
  const { sessions, cases, spentUsd } = state;
  if (cases === 0 && spentUsd >= rule.zeroCaseUsd) {
    return { rule: "stop-zero-case-usd", sessions, cases, spentUsd, projectedUsd: null };
  }
  if (sessions < rule.afterSessions || !Number.isFinite(max) || cases >= max) return undefined;
  const projected = cases === 0 ? Number.POSITIVE_INFINITY : (max * spentUsd) / cases;
  if (projected <= rule.projectedUsd) return undefined;
  return {
    rule: "stop-projected-usd",
    sessions,
    cases,
    spentUsd,
    projectedUsd: Number.isFinite(projected) ? projected : null,
  };
}

/** The most windows of a session the case reader reads, one call each, until it proposes a step. */
export const READER_WINDOWS = 6;
/** The most messages the case reader reads from the skill's read on. */
export const READER_MESSAGES = 40;
/** The most characters of rendered messages the case reader reads from the skill's read on. */
export const READER_CHARS = 120_000;
/** The most messages before the skill's read that the case reader reads, for its search for a
 *  restatement of the line. */
export const READER_EARLIER_MESSAGES = 20;
/** The most characters of rendered messages before the skill's read that the case reader reads. */
export const READER_EARLIER_CHARS = 40_000;
/** The most characters of one window of the restatement check (the `restate` reader). */
export const RESTATE_CHARS = 100_000;
/** The most windows the restatement check reads for one case: a case whose messages before the
 *  step need more is not checked whole, and is dropped. */
export const RESTATE_WINDOWS = 10;
/** A message longer than this goes to the restatement check in parts this long at most, each
 *  starting RESTATE_OVERLAP characters before the end of the one before, so a sentence cut at the
 *  end of one part is whole in the next. */
export const RESTATE_PART_CHARS = 50_000;
export const RESTATE_OVERLAP = 1_000;
/** The most first user messages the second tasks reader call reads. */
export const TASK_MESSAGES = 60;
/** The longest one try of a reader call may take: a reply with a long thinking budget is slow. */
const READER_TIMEOUT_MS = 600_000;
const DAY_MS = 86_400_000;

/** Why a session gives no case. */
export const DROP_REASONS = {
  "not-in-store": "the store holds no file of the session's hash",
  "pull-failed": "the transcript pull failed or returned nothing",
  unparseable: "a line of the transcript is not one entry",
  "no-header": "the transcript has no session header with a start time",
  "no-read": "no read of a skill:// file of the skill succeeded",
  "not-cut-copy":
    "attribute.ts's rules (the prefix table, a checkout, the store's context) do not place the skill's first read at the cut repository's copy, or the profile installs no copy of it",
  "no-role":
    "a Legion session's role is in none of the forms the builder reads (module comment, The role)",
  "bad-session-init":
    "a subagent's session_init line holds no system prompt, no tool names or no agent name",
  "reader-failed": "the case reader's every reply was refused",
  "reader-error": "the case reader's call failed",
  "no-step": "the case reader found no step the skill governs",
  "no-common-line":
    "the session's own successful reads of the skill's files hold no whole line of the base text, so the case reader has no line to offer",
  "step-not-after-read": "the case reader's step is at or before the skill's read",
  "line-not-in-base": "the proposed line is not a whole line of the base text",
  "line-not-in-read": "no successful read result of the line's file before the step holds the line",
  "read-before-compaction":
    "a skill read precedes a compaction (or a context reset) that precedes the step, or the case reader found no step before the first one after the read (a step after it would be so)",
  "compaction-unsupported":
    "a compaction before the step is of a kind this builder does not rebuild",
  "line-quoted": "an assistant message before the step quotes the line",
  "history-restates-line":
    "a message before the step states the line's instruction in other words (the case reader's or the restatement check's finding)",
  "restatement-check-incomplete":
    "the restatement check could not read every message before the step the case keeps (too many windows, or a window's reader failed)",
  "audit-breaks": "the audit, which reads no criterion, finds that the real step breaks the line",
  "audit-not-governed":
    "the audit, which reads no criterion, finds that the line does not govern the real step",
  "audit-failed": "the audit reader's every reply was refused, or its call failed",
  "no-model": "no model_change or thinking_level_change before the step",
  "model-not-served":
    "no route of the gateway serves the session's model before the step (its id is the id of none of the configuration's servedModels), or none of its models from the skill's read on",
  "read-by-path": "a read before the step reads a file of the skill by its path or with a selector",
  "profile-failed": "a GitHub read the profile needs failed",
} as const;
export type DropReason = keyof typeof DROP_REASONS;

/** The role that works each phase (packages/daemon/internal/phase/phase.go, `RoleFor`). */
export const PHASE_ROLES: Readonly<Record<string, string>> = {
  admitted: "architect",
  accepting: "architect",
  planning: "planner",
  implementing: "implementer",
  retro: "implementer",
  testing: "tester",
  reviewing: "reviewer",
  merging: "merger",
};
/** The daemon's first line to a phase worker (workflow/effects.go, `continueLine`). */
const CONTINUE_LINE = /^Continue [\s\S]*?\. Issue: [A-Z][A-Z0-9]*-\d+\. Phase: ([a-z_]+)\./;
/** The daemon's start of a tree's root architect: every shape of `ArchitectStartTask`
 *  (workflow/architect_start.go) says `<KEY> is yours and`, after a person's decision if any. */
const ROOT_START = /(?:^|\n\n)([A-Z][A-Z0-9]*-\d+) is yours and /;
/** The daemon's first message to a project's controller (daemon/controller.go). */
const CONTROLLER_START = "Legion controller start: ";
/** The reasons of the daemon's catch-up notice to an architect (workflow/catch_up.go): of a
 *  tree's root, and of a child it owns. */
const CATCH_UP_ROOT =
  /the tree of ([A-Z][A-Z0-9]*-\d+) at generation \d+ as the daemon records it now/;
const CATCH_UP_CHILD = /the issues under ([A-Z][A-Z0-9]*-\d+), which you own, at generation \d+/;
/** A role claim's token (claim/token.go): `legion-<project>-<issue project>-<number>-<role>`, or
 *  `legion-<project>-controller`. */
const ROLE_TOKEN =
  /^legion-[a-z0-9]+-([a-z][a-z0-9]*)-(\d+)-(architect|planner|implementer|tester|reviewer|merger)$/;
const CONTROLLER_TOKEN = /^legion-[a-z0-9]+-controller$/;
/** The issue of a Legion session's workspace, in its transcript's path: Oh My Pi keeps a session
 *  under a directory named after its working directory, `--legion-workspaces-<owner>-<name>-<issue,
 *  lowercased>--`. */
const WORKSPACE_DIR = /\/--legion-workspaces-[A-Za-z0-9._-]*?-([a-z][a-z0-9]*)-(\d+)--\//;
/** The custom message kinds Oh My Pi 18.18.6 leaves out of the model's messages (plan mode's). */
const LEFT_OUT_CUSTOM = new Set(["prewalk-plan", "vibe-mode-context"]);
/** Where a Legion pod's worker image installs its Oh My Pi packages (worker.Dockerfile). */
const IMAGE_ROOT = "/opt/legion/";
/** Claude Code's settings in its config directory, as copies.ts reads them (its claude-settings
 *  manifest), and the user memory file beside them, which Oh My Pi reads as its user-level context
 *  file (discovery/claude.ts at the pinned tag). */
const CLAUDE_SETTINGS = ".claude/settings.json";
const USER_CONTEXT_FILE = ".claude/CLAUDE.md";
/** A Legion workspace: `/legion/workspaces/<owner>/<name>/<workspace>`. */
const LEGION_WORKSPACE = /^\/legion\/workspaces\/([A-Za-z0-9-]+)\/([A-Za-z0-9._-]+)(?:\/|$)/;
/** What stands in for each secret the scan takes out. */
export const SECRET_REMOVED = "[secret removed]";

/** What `main` uses of the world. */
export interface BuildIo {
  /** Writes `text` to standard output as it is. */
  out(text: string): void;
  /** Writes one line to standard error. */
  err(line: string): void;
  /** Runs gh (copies.ts's `spawnGh` in a real run). */
  readonly gh: Gh;
  /** Milliseconds on a clock that only goes forward (`performance.now()` in a real run): what
   *  build-tasks' candidate search measures its own time by. */
  now(): number;
}

/** The directory a build pulls transcripts into, `<out>.spool`, made empty: one file for each
 *  session open, removed once the session is decided, and the directory once the build ends. A
 *  build that is killed leaves it for the next run of the same `--out`, which empties it. */
function spoolDir(out: string): string {
  const dir = `${out}.spool`;
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  return dir;
}

/** Refuses a build's `--out` inside a git or jj checkout (production-dispatch.ts's
 *  `outRefusal`): it and its spool (`spoolDir`) hold real session text, which must stay out of a
 *  checkout. */
function refuseOutInCheckout(out: string): void {
  for (const file of [out, `${out}.spool`]) {
    const refusal = outRefusal(file);
    if (refusal !== undefined) throw new Refused(refusal);
  }
}

/** A file or directory a build reads, and the SHA-256 of what it read there. */
const Digested = z.strictObject({ path: z.string(), sha256: z.string().regex(/^[0-9a-f]{64}$/) });
type Digested = z.infer<typeof Digested>;

/** The SHA-256 of the files `files` of `dir` (each a path inside it), each with its name and
 *  length, so two sets of files differ whenever a name, a length or a byte does. A file that is
 *  not there counts as one of no bytes, marked missing. */
function digestOf(dir: string, files: readonly string[]): Digested {
  const hash = createHash("sha256");
  for (const file of files) {
    const where = path.join(dir, file);
    const bytes = existsSync(where) ? readFileSync(where) : undefined;
    hash.update(`${file}\0${bytes === undefined ? "missing" : bytes.length}\0`);
    if (bytes !== undefined) hash.update(bytes);
  }
  return { path: dir, sha256: hash.digest("hex") };
}

/** One model's spend as a summary records it. */
const ModelSpendRecord = z.strictObject({
  calls: z.number(),
  input: z.number(),
  output: z.number(),
  cacheRead: z.number(),
  cacheWrite: z.number(),
  reasoning: z.number(),
  usd: z.number(),
});
/** The readers' spend as a summary records it (gateway.ts's `Spend`). */
const SpendRecord = z.strictObject({
  models: z.record(z.string(), ModelSpendRecord),
  usd: z.number(),
});

/** The spend of two runs of one build: each model's calls, tokens and dollars summed. */
function addSpend(a: Spend, b: Spend): Spend {
  const models = { ...a.models };
  for (const [model, spend] of Object.entries(b.models)) {
    models[model] = addModelSpend(models[model], spend);
  }
  return { models, usd: a.usd + b.usd };
}

/** No spend. */
const NO_SPEND: Spend = { models: {}, usd: 0 };

/** The text of the JSON file `file`, parsed, or a refusal naming it. */
function readJson(file: string): unknown {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Refused(`${file} cannot be read as JSON: ${messageFor(error)}`);
  }
}

// ---------------------------------------------------------------------------------------------
// The store

/** The rows of `query` run by the store command `command` on the database `db`, as eachStoreRow
 *  reads them. */
async function storeRows(command: string, db: string, query: string): Promise<unknown[]> {
  const rows: unknown[] = [];
  await eachStoreRow(command, db, query, (row) => {
    rows.push(row);
  });
  return rows;
}

/**
 * Runs `query` by the store command `command` on the database `db` and gives each row to `visit`
 * as the command prints it, read by psql-rows.ts's `PsqlTable`, so no more than one row's text is
 * held. A command that fails is reported first, with its first error line; then a fault of its
 * output; then a throw of `visit`, after which no row is given. Returns the rows the table holds.
 */
async function eachStoreRow(
  command: string,
  db: string,
  query: string,
  visit: (row: unknown) => void
): Promise<number> {
  const child = Bun.spawn([command, db], {
    stdin: new Blob([query]),
    stdout: "pipe",
    stderr: "pipe",
  });
  const stderr = new Response(child.stderr).text();
  const table = new PsqlTable(visit);
  for await (const chunk of child.stdout) table.write(chunk);
  const code = await child.exited;
  if (code !== 0) {
    const first = (await stderr).split("\n").find((each) => each.trim() !== "") ?? "";
    throw new Error(`the store command exited ${code}: ${first.slice(0, 300)}`);
  }
  return table.end();
}

/** Pulls the file `filePath` of the store `db` into the file `spool`, PULL_PAGE rows a call, each
 *  row checked by psql-rows.ts's `PullCheck` and its bytes written as it comes (`eachStoreRow`),
 *  so one row of at most 1 MiB is held at a time however long the file. Returns the bytes it
 *  wrote and the calls it took. */
export async function pullTranscript(
  command: string,
  db: string,
  filePath: string,
  spool: string
): Promise<{ bytes: number; calls: number }> {
  const fd = openSync(spool, "w");
  try {
    const check = new PullCheck();
    const write = (row: unknown) => {
      const text = check.take(row);
      for (let at = 0; at < text.length; ) at += writeSync(fd, text, at);
    };
    for (let calls = 1; ; calls += 1) {
      const page = await eachStoreRow(command, db, pullSql(filePath, check.last), write);
      if (page < PULL_PAGE) return { bytes: check.bytes, calls };
    }
  } finally {
    closeSync(fd);
  }
}

/** The file hash count.sql and sessions.sql name a file by: md5 of its path, 16 hex characters. */
export function fileHash(filePath: string): string {
  return createHash("md5").update(filePath).digest("hex").slice(0, 16);
}

const SessionRow = z.strictObject({ path: z.string().min(1), hash: z.string() });

/** The paths of the files of `hashes` in the store `db`, by hash, in one sessions.sql query (none
 *  for no hash): the query reads the store's whole file table once, whatever the hashes. */
async function resolvePaths(
  command: string,
  db: string,
  hashes: readonly string[]
): Promise<{ paths: Map<string, string>; queries: number }> {
  const paths = new Map<string, string>();
  if (hashes.length === 0) return { paths, queries: 0 };
  for (const row of await storeRows(command, db, sessionsSql(hashes))) {
    const { path: filePath, hash } = SessionRow.parse(row);
    paths.set(hash, filePath);
  }
  return { paths, queries: 1 };
}

// ---------------------------------------------------------------------------------------------
// The transcript

/** The most bytes the transcript reader reads from a spool at once. */
export const SPOOL_CHUNK = 1 << 20;

/** The entry of a transcript line whose JSON is `value`, the line's number `line`: a message's
 *  string `content` made one text block, as Oh My Pi reads it. Throws when it is no entry. */
function entryOf(value: unknown, line: number): SessionEntry {
  if (isObject(value) && isObject(value.message) && typeof value.message.content === "string") {
    value.message.content = [{ type: "text", text: value.message.content }];
  }
  const parsed = SessionEntry.safeParse(value);
  if (!parsed.success) {
    throw new Error(`line ${line} is not a transcript entry: ${parsed.error.message}`);
  }
  return parsed.data;
}

/** Each line of the file `file` in order, split at each newline as `text.split("\n")` splits it:
 *  its text, its number, and the offset and length of its bytes. The file is read `chunk` bytes
 *  at a time, so one chunk and one line are held. The text after the last newline, when there is
 *  any, comes last, with `last` set. */
function eachLine(
  file: string,
  chunk: number,
  visit: (text: string, line: number, offset: number, length: number, last: boolean) => void
): void {
  const fd = openSync(file, "r");
  try {
    const buffer = Buffer.alloc(chunk);
    let carry: Buffer[] = [];
    let carried = 0;
    let start = 0;
    let position = 0;
    let line = 0;
    for (;;) {
      const read = readSync(fd, buffer, 0, chunk, position);
      if (read === 0) break;
      position += read;
      let from = 0;
      for (;;) {
        const newline = buffer.indexOf(0x0a, from);
        if (newline < 0 || newline >= read) break;
        const piece = buffer.subarray(from, newline);
        const bytes = carried === 0 ? piece : Buffer.concat([...carry, piece]);
        line += 1;
        visit(bytes.toString("utf8"), line, start, bytes.length, false);
        start += bytes.length + 1;
        carry = [];
        carried = 0;
        from = newline + 1;
      }
      if (from < read) {
        carry.push(Buffer.from(buffer.subarray(from, read)));
        carried += read - from;
      }
    }
    if (carried > 0) {
      visit(Buffer.concat(carry).toString("utf8"), line + 1, start, carried, true);
    }
  } finally {
    closeSync(fd);
  }
}

/** The bytes `length` of the file `file` from `offset`. */
function bytesAt(file: string, offset: number, length: number): Buffer {
  const bytes = Buffer.alloc(length);
  const fd = openSync(file, "r");
  try {
    for (let at = 0; at < length; ) {
      const read = readSync(fd, bytes, at, length - at, offset + at);
      if (read === 0) throw new Error(`${file} ends before byte ${offset + length}`);
      at += read;
    }
  } finally {
    closeSync(fd);
  }
  return bytes;
}

/** The entries of the session's current branch, root first: the parent chain of its last entry
 *  that has an id. The header and an entry with no id are no part of it. */
export function currentBranch(entries: readonly SessionEntry[]): SessionEntry[] {
  const inBranch = (entry: SessionEntry) =>
    typeof entry.id === "string" && entry.type !== "session";
  const byId = new Map<string, SessionEntry>();
  for (const entry of entries) if (inBranch(entry)) byId.set(String(entry.id), entry);
  const chain: SessionEntry[] = [];
  const seen = new Set<string>();
  let at = entries.findLast(inBranch);
  while (at !== undefined && !seen.has(String(at.id))) {
    chain.push(at);
    seen.add(String(at.id));
    at = typeof at.parentId === "string" ? byId.get(at.parentId) : undefined;
  }
  return chain.reverse();
}

/** An entry's time as epoch milliseconds, or 0. */
function entryTime(entry: SessionEntry): number {
  const ms = Date.parse(entry.timestamp ?? "");
  return Number.isNaN(ms) ? 0 : ms;
}

/** Whether an assistant message is one Oh My Pi leaves out of the model's messages: a retry's
 *  recovery, or an error that holds no content. */
function leftOutAssistant(message: SessionMessage): boolean {
  if (message.retryRecovery === true) return true;
  if (message.stopReason !== "error") return false;
  return !(message.content ?? []).some((block) =>
    block.type === "toolCall"
      ? true
      : block.type === "text"
        ? (block.text ?? "").trim() !== ""
        : block.type === "thinking" &&
          (String(block.thinking ?? "").trim() !== "" ||
            String(block.thinkingSignature ?? "").trim() !== "")
  );
}

/** The message the model reads for `entry`, or undefined for an entry it reads none of. */
export function messageOf(entry: SessionEntry): SessionMessage | undefined {
  if (entry.type === "message" && entry.message !== undefined) {
    return entry.message.role === "assistant" && leftOutAssistant(entry.message)
      ? undefined
      : entry.message;
  }
  if (entry.type === "custom_message" && !LEFT_OUT_CUSTOM.has(entry.customType ?? "")) {
    const content =
      typeof entry.content === "string" ? [{ type: "text", text: entry.content }] : entry.content;
    return SessionMessage.parse({
      role: "custom",
      customType: entry.customType,
      content,
      display: entry.display,
      details: entry.details,
      attribution: entry.attribution,
      timestamp: entryTime(entry),
    });
  }
  if (entry.type === "branch_summary" && typeof entry.summary === "string") {
    return {
      role: "branchSummary",
      summary: entry.summary,
      fromId: entry.fromId,
      timestamp: entryTime(entry),
    };
  }
  return undefined;
}

/** Where a line is in a spool: its offset, its length in bytes and its number. */
interface LineAt {
  readonly offset: number;
  readonly length: number;
  readonly line: number;
}

/** One message of the branch: its light form (`slimMessage`), its entry's place in the branch,
 *  and its line in the spool. */
interface Item {
  readonly slim: SessionMessage;
  readonly at: number;
  readonly line: LineAt;
}

/** A transcript read from its spool (`readTranscript`). Only the entries the builder reads whole
 *  are held whole (`heldWhole`); every other entry is light, its type, id, parent, time and custom
 *  type, and is read again from the spool when it is wanted whole. */
interface Transcript {
  /** Every entry, in order, whole or light. */
  readonly entries: readonly SessionEntry[];
  /** The session's current branch of `entries`. */
  readonly branch: readonly SessionEntry[];
  /** The branch's messages, in order. */
  readonly items: readonly Item[];
  /** The whole message of item `index`, read again from the spool. */
  readonly message: (index: number) => SessionMessage;
  /** The whole entry of the branch at `at`. */
  readonly entry: (at: number) => SessionEntry | undefined;
}

/** Whether a transcript holds `entry` whole: an entry the builder reads beyond its type, id,
 *  parent and time without asking for it (the header, a subagent's `session_init`, a model or
 *  thinking level change, a context reset, a recorded system prompt's digest and an Envoy role
 *  claim). A message's entry, a compaction and every other kind are held light. */
function heldWhole(entry: SessionEntry): boolean {
  switch (entry.type) {
    case "session":
    case "session_init":
    case "model_change":
    case "thinking_level_change":
    case "reset_boundary":
      return true;
    case "custom":
      return entry.customType === "system-prompt-digest" || entry.customType === "envoy-role-claim";
    default:
      return false;
  }
}

/** `message` with only what a scan of every message of a session reads (skillReads, shownReads,
 *  judge.ts's skillReadResults, and the roles sessionRole looks for): its role, tool, call id,
 *  error mark and custom type; each tool call, a `read` with its arguments and any other without;
 *  and, for a tool result, its first block, holding only the `[Skill file: …]` line it starts
 *  with. */
function slimMessage(message: SessionMessage): SessionMessage {
  const content = message.content?.flatMap((block, n): NonNullable<SessionMessage["content"]> => {
    if (block.type === "toolCall") {
      return [
        {
          type: "toolCall",
          id: block.id,
          name: block.name,
          ...(block.name === "read" ? { arguments: block.arguments } : {}),
        },
      ];
    }
    if (n !== 0 || message.role !== "toolResult") return [];
    const header = skillHeaderPath(block.text ?? "");
    return [
      {
        type: block.type,
        ...(header === undefined ? {} : { text: `${skillFileHeader(header)}\n` }),
      },
    ];
  });
  return {
    role: message.role,
    toolName: message.toolName,
    toolCallId: message.toolCallId,
    isError: message.isError,
    customType: message.customType,
    ...(content === undefined ? {} : { content }),
  };
}

/**
 * The transcript in the file `spool`, read line by line (`eachLine`, `chunk` bytes at a time): the
 * text after its last newline is a line still being written when it does not parse, and is left
 * out; any other line that is not one entry throws. An entry `heldWhole` names is held whole; any
 * other is held light, with where its line is, and a message's with its light form
 * (`slimMessage`), so a session's memory follows its number of entries, not their text.
 */
export function readTranscript(spool: string, chunk = SPOOL_CHUNK): Transcript {
  const entries: SessionEntry[] = [];
  const lines = new Map<SessionEntry, { line: LineAt; slim: SessionMessage | undefined }>();
  eachLine(spool, chunk, (text, line, offset, length, last) => {
    if (text.trim() === "") return;
    let value: unknown;
    try {
      value = JSON.parse(text);
    } catch (error) {
      if (last) return;
      throw new Error(`line ${line} is not JSON: ${messageFor(error)}`);
    }
    const entry = entryOf(value, line);
    if (heldWhole(entry)) {
      entries.push(entry);
      return;
    }
    const light: SessionEntry = {
      type: entry.type,
      id: entry.id,
      parentId: entry.parentId,
      timestamp: entry.timestamp,
      ...(entry.customType === undefined ? {} : { customType: entry.customType }),
    };
    const message = messageOf(entry);
    entries.push(light);
    lines.set(light, {
      line: { offset, length, line },
      slim: message === undefined ? undefined : slimMessage(message),
    });
  });
  const branch = currentBranch(entries);
  const items: Item[] = [];
  for (const [at, each] of branch.entries()) {
    const held = lines.get(each);
    if (held?.slim !== undefined) items.push({ slim: held.slim, at, line: held.line });
  }
  const whole = ({ offset, length, line }: LineAt): SessionEntry =>
    entryOf(JSON.parse(bytesAt(spool, offset, length).toString("utf8")), line);
  const message = (index: number): SessionMessage => {
    const item = items[index];
    if (item === undefined) throw new Error(`the transcript has no message ${index}`);
    const read = messageOf(whole(item.line));
    if (read === undefined)
      throw new Error(`line ${item.line.line} of the transcript holds no message`);
    return read;
  };
  const entryAt = (at: number): SessionEntry | undefined => {
    const light = branch[at];
    const held = light === undefined ? undefined : lines.get(light);
    return held === undefined ? light : whole(held.line);
  };
  return { entries, branch, items, message, entry: entryAt };
}

/** The text of a message's text blocks, joined. */
function messageText(message: SessionMessage): string {
  return (message.content ?? [])
    .flatMap((block) => (block.type === "text" ? [block.text ?? ""] : []))
    .join("\n");
}

/** A skill read of the messages: the read result's index, its call's index, the file it read, and
 *  the path its `[Skill file: …]` line names. */
interface SkillRead {
  readonly index: number;
  readonly call: number;
  readonly file: string;
  readonly header: string | undefined;
  readonly ok: boolean;
}

/** Each read result of a `skill://<skill>` file in `messages`, in order. */
function skillReads(messages: readonly SessionMessage[], skill: string): SkillRead[] {
  const targets = readTargets(messages);
  const calls = new Map<string, number>();
  for (const [index, message] of messages.entries()) {
    if (message.role !== "assistant") continue;
    for (const block of message.content ?? []) {
      if (block.type === "toolCall" && block.id !== undefined) calls.set(block.id, index);
    }
  }
  const reads: SkillRead[] = [];
  for (const [index, message] of messages.entries()) {
    if (message.role !== "toolResult" || message.toolName !== "read") continue;
    const target = targets.get(message.toolCallId ?? "");
    const file = target === undefined ? undefined : skillReadFile(skill, target);
    if (file === undefined) continue;
    const header = skillHeaderPath(message.content?.[0]?.text ?? "");
    reads.push({
      index,
      call: calls.get(message.toolCallId ?? "") ?? index,
      file,
      header,
      ok: message.isError !== true && header !== undefined,
    });
  }
  return reads;
}

/** A successful read result whose first line is `[Skill file: <path>]`: its index in the
 *  messages, that path, and the skill its call names (`skill://<skill>`), when it names one. */
interface ShownRead {
  readonly index: number;
  readonly header: string;
  readonly skill: string | undefined;
}

/** Each successful read result of `messages` that shows a skill file's path, any skill's. */
function shownReads(messages: readonly SessionMessage[]): ShownRead[] {
  const targets = readTargets(messages);
  return messages.flatMap((message, index) => {
    if (message.role !== "toolResult" || message.toolName !== "read" || message.isError === true) {
      return [];
    }
    const header = skillHeaderPath(message.content?.[0]?.text ?? "");
    if (header === undefined) return [];
    const target = targets.get(message.toolCallId ?? "") ?? "";
    return [{ index, header, skill: /^skill:\/\/([^/:?#]+)/.exec(target)?.[1] }];
  });
}

/** The lines of each base file in `files` that a successful read of it in `reads` holds whole
 *  (its text after the `[Skill file: …]` line; each line trimmed, as the builder's check of a
 *  proposed line compares them; blank lines aside), each with its number in the base file. A file
 *  no read holds a line of is not offered. `message` gives a read's whole message by its index. */
function offeredFiles(
  files: readonly { readonly path: string; readonly text: string }[],
  reads: readonly SkillRead[],
  message: (index: number) => SessionMessage
): CaseInput["files"] {
  const texts = new Map(
    reads
      .filter((read) => read.ok)
      .map((read): [number, string] => [read.index, message(read.index).content?.[0]?.text ?? ""])
  );
  return files.flatMap((file) => {
    const held = new Set(
      reads
        .filter((read) => read.ok && read.file === file.path)
        .flatMap((read) => textLines(stripSkillHeader(texts.get(read.index) ?? "")))
        .map((line) => line.trim())
    );
    const lines = textLines(file.text).flatMap((text, index) =>
      text.trim() !== "" && held.has(text.trim()) ? [{ number: index + 1, text }] : []
    );
    return lines.length === 0 ? [] : [{ path: file.path, lines }];
  });
}

/** Why a read call of `messages` reads a file of the skill in a way arms.ts does not rebuild, or
 *  undefined: a `skill://<skill>` URL with a selector, or a path (its selector left aside) that
 *  ends `/<skill>/SKILL.md`, holds `skills/<skill>/`, or is under one of `installedDirs`. */
function readByPath(
  messages: readonly SessionMessage[],
  skill: string,
  installedDirs: readonly string[]
): string | undefined {
  for (const [index, message] of messages.entries()) {
    for (const { target } of readCalls([{ message }])) {
      if (readsSkillUrl(target, skill)) {
        if (skillReadFile(skill, target) === undefined) {
          return `message ${index} reads ${target}, with a selector`;
        }
        continue;
      }
      if (/^[a-z][a-z0-9+.-]*:\/\//i.test(target)) continue;
      const file = target.replace(/:[^/]*$/, "");
      if (
        file.endsWith(`/${skill}/SKILL.md`) ||
        file === `${skill}/SKILL.md` ||
        file.includes(`skills/${skill}/`) ||
        installedDirs.some((dir) => file.startsWith(`${dir}/`))
      ) {
        return `message ${index} reads ${target} by its path`;
      }
    }
  }
  return undefined;
}

/** The text a line is quoted by: the line less a leading list, heading or quote mark, trimmed. */
function quotedText(line: string): string {
  const bare = line
    .trim()
    .replace(/^(?:[-*+]|\d+[.)]|#{1,6}|>)\s+/, "")
    .trim();
  return bare === "" ? line.trim() : bare;
}

/** Every string inside `value`. */
function strings(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(strings);
  if (isObject(value)) return Object.values(value).flatMap(strings);
  return [];
}

/** The index of the first assistant message of `messages` that quotes `line`, or undefined: its
 *  text, its thinking or a string of its tool calls' arguments holds the line. */
function quotingMessage(messages: readonly SessionMessage[], line: string): number | undefined {
  const quote = quotedText(line);
  const found = messages.findIndex(
    (message) =>
      message.role === "assistant" &&
      (message.content ?? []).some((block) =>
        strings([block.text, block.thinking, block.arguments]).some((text) => text.includes(quote))
      )
  );
  return found === -1 ? undefined : found;
}

/** A message as the restatement check reads it: judge.ts's `renderMessage` (its role, text and
 *  tool calls), then its thinking, which the model can be sent back, and its summary, for a
 *  compaction's or a branch's. Nothing is cut. */
function restatementText(message: SessionMessage): string {
  const thinking = (message.content ?? []).flatMap((block) =>
    block.type === "thinking" && typeof block.thinking === "string" && block.thinking.trim() !== ""
      ? [`[thinking]\n${block.thinking}`]
      : []
  );
  const summary = typeof message.summary === "string" ? [message.summary] : [];
  return [renderMessage(message), ...thinking, ...summary].join("\n");
}

/** The windows the restatement check reads of `messages` (a case's messages before its step),
 *  less the indices of `leftOut` (the skill's own read results): each message whole, in order, a
 *  message longer than RESTATE_PART_CHARS in overlapping parts; each window at most RESTATE_CHARS
 *  characters, or one part. With each window, the indices of the messages whose last part it
 *  holds; and how many messages the check reads. */
export function restatementWindows(
  messages: readonly SessionMessage[],
  leftOut: ReadonlySet<number>
): { windows: { messages: CaseInput["messages"]; completes: number[] }[]; checked: number } {
  const pieces: { piece: CaseInput["messages"][number]; last: boolean }[] = [];
  let checked = 0;
  for (const [index, message] of messages.entries()) {
    if (leftOut.has(index)) continue;
    checked += 1;
    const role = message.role ?? "message";
    const text = restatementText(message);
    if (text.length <= RESTATE_PART_CHARS) {
      pieces.push({ piece: { index, role, text }, last: true });
      continue;
    }
    const stride = RESTATE_PART_CHARS - RESTATE_OVERLAP;
    const count = Math.ceil((text.length - RESTATE_PART_CHARS) / stride) + 1;
    for (let part = 0; part < count; part += 1) {
      const body = text.slice(part * stride, part * stride + RESTATE_PART_CHARS);
      pieces.push({
        piece: { index, role, text: `[part ${part + 1} of ${count} of this message]\n${body}` },
        last: part === count - 1,
      });
    }
  }
  const windows: { messages: CaseInput["messages"]; completes: number[] }[] = [];
  let chars = 0;
  for (const { piece, last } of pieces) {
    let window = windows.at(-1);
    if (window === undefined || chars + piece.text.length > RESTATE_CHARS) {
      window = { messages: [], completes: [] };
      windows.push(window);
      chars = 0;
    }
    window.messages.push(piece);
    chars += piece.text.length;
    if (last) window.completes.push(piece.index);
  }
  return { windows, checked };
}

const CompactionEntry = z.looseObject({
  type: z.literal("compaction"),
  id: z.string(),
  summary: z.string(),
  firstKeptEntryId: z.string(),
  tokensBefore: z.number(),
  timestamp: z.string(),
});

/** The model's messages before the branch entry at `stepAt`: the items before it, or, after the
 *  branch's last compaction or context reset before it, what Oh My Pi gives the model after that.
 *  A drop when a skill read (at `firstReadAt`) precedes that compaction or reset, or when the
 *  compaction is of a kind this file does not rebuild. */
function messagesBefore(
  t: Transcript,
  stepAt: number,
  firstReadAt: number
): { messages: SessionMessage[] } | { drop: DropReason; detail: string } {
  let boundary = -1;
  for (let at = stepAt - 1; at >= 0; at -= 1) {
    const type = t.branch[at]?.type;
    if (type === "compaction" || type === "reset_boundary") {
      boundary = at;
      break;
    }
  }
  /** Each item whose entry is in the branch from `from` (inclusive) to `to`, whole. */
  const whole = (from: number, to: number) =>
    t.items.flatMap((item, index) => (item.at >= from && item.at < to ? [t.message(index)] : []));
  const before = (from: number) => whole(from + 1, stepAt);
  if (boundary < 0) return { messages: before(-1) };
  const entry = t.entry(boundary);
  if (firstReadAt < boundary) {
    return {
      drop: "read-before-compaction",
      detail: `the skill read at branch entry ${firstReadAt} precedes the ${entry?.type} at entry ${boundary}`,
    };
  }
  if (entry?.type === "reset_boundary") return { messages: before(boundary) };
  const compaction = CompactionEntry.safeParse(entry);
  const kept = compaction.success
    ? t.branch.findIndex(
        (each, at) => at < boundary && each.id === compaction.data.firstKeptEntryId
      )
    : -1;
  // A compaction this file does not rebuild the messages after, and why.
  let unsupported: string | undefined;
  if (entry === undefined || !compaction.success)
    unsupported = "is not a compaction this file reads";
  else if (kept < 0) unsupported = "keeps no entry of the branch";
  else if (entry.preserveData !== undefined || entry.providerReplayThroughEntryId !== undefined)
    unsupported = "keeps provider data";
  else if (entry.method === "snapcompact") unsupported = "is a snapcompact compaction";
  else if (isObject(entry.details) && entry.details.kind === "experimental-context-rollover")
    unsupported = "is an experimental context rollover";
  if (entry === undefined || !compaction.success || unsupported !== undefined) {
    return {
      drop: "compaction-unsupported",
      detail: `the compaction at branch entry ${boundary} ${unsupported}`,
    };
  }
  const summary = SessionMessage.parse({
    role: "compactionSummary",
    summary: compaction.data.summary,
    shortSummary: entry.shortSummary,
    tokensBefore: compaction.data.tokensBefore,
    tokensAfter: entry.tokensAfter,
    method: entry.method,
    warning: entry.warning,
    timestamp: entryTime(entry),
  });
  const keptMessages = whole(kept, boundary);
  return { messages: [summary, ...keptMessages, ...before(boundary)] };
}

/** The last entry of `type` in the branch before `at` that `pick` reads a value from. */
function lastBefore<T>(
  branch: readonly SessionEntry[],
  at: number,
  pick: (entry: SessionEntry) => T | undefined
): T | undefined {
  for (let index = at - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    const value = entry === undefined ? undefined : pick(entry);
    if (value !== undefined) return value;
  }
  return undefined;
}

/** The model a `model_change` entry names, or undefined for any other entry. */
function modelChange(entry: SessionEntry): string | undefined {
  return entry.type === "model_change" && typeof entry.model === "string" ? entry.model : undefined;
}

/** The thinking level a `thinking_level_change` entry names, or undefined for any other entry. */
function thinkingChange(entry: SessionEntry): string | undefined {
  return entry.type === "thinking_level_change" && typeof entry.thinkingLevel === "string"
    ? entry.thinkingLevel
    : undefined;
}

/** `model`'s id and whether a route of the gateway serves it, by the read count's rule
 *  (attribute.ts's `sessionModel`): a thinking suffix left out, its id (the text after the last
 *  `/`, whatever gateway or provider names it) is one of `servedIds`. */
function served(model: string, servedIds: ReadonlySet<string>): { id: string; served: boolean } {
  const known = sessionModel({ model, source: "model_change" }, servedIds);
  return { id: modelId(known?.model ?? model), served: known?.served === true };
}

/** The detail of a `model-not-served` drop: each model and its id. */
function notServed(models: readonly string[], servedIds: ReadonlySet<string>): string {
  const named = models.map((model) => `${model} (id ${served(model, servedIds).id})`).join(", ");
  return models.length === 1
    ? `${named}: no route of the gateway serves it; no servedModels entry has its id`
    : `the session's models from the skill's read on, ${named}: no route of the gateway serves any of them; no servedModels entry has their ids`;
}

/** `model` as attribute.ts's models table writes a session's model (`sessionModel`): as the
 *  session's file names it, its provider path kept and a thinking suffix left out. */
function modelForm(model: string): string {
  return sessionModel({ model, source: "model_change" }, new Set())?.model ?? model;
}

/** The model the first request of the session `t` ran on, as `build-tasks --model` compares it:
 *  the last `model_change` of its branch before its first assistant message (before the branch's
 *  end when it has none), as build-cases reads a session's model before a point of it, in the
 *  models table's form (`modelForm`); undefined when no model_change comes before it. */
function firstReplyModel(t: Transcript): string | undefined {
  const reply = t.items.find((item) => item.slim.role === "assistant");
  const model = lastBefore(t.branch, reply?.at ?? t.branch.length, modelChange);
  return model === undefined ? undefined : modelForm(model);
}

// ---------------------------------------------------------------------------------------------
// The session's role

/** A subagent's `session_init` line: the system prompt and tools its session was given. */
const SessionInit = z.looseObject({
  type: z.literal("session_init"),
  agent: z.string().regex(/^[^:\s]+$/),
  systemPrompt: z.array(z.string()).min(1),
  tools: z.array(z.string().min(1)).min(1),
});
type SessionInit = z.infer<typeof SessionInit>;

/** The `session_init` line of a subagent's transcript, `undefined` for a session that has none,
 *  or `null` when it has one that is not of that shape. */
export function sessionInit(entries: readonly SessionEntry[]): SessionInit | null | undefined {
  const entry = entries.find((each) => each.type === "session_init");
  if (entry === undefined) return undefined;
  const parsed = SessionInit.safeParse(entry);
  return parsed.success ? parsed.data : null;
}

const RoleClaim = z.looseObject({ data: z.looseObject({ role: z.string() }) });

/** The role of a Legion session as its context names it (`architect-root` for a tree's root
 *  architect), with how it was read; or why none was, with the session's first words (at most 80
 *  characters). The forms, in order (the module comment, "The role"). */
export function sessionRole(
  branch: readonly SessionEntry[],
  messages: readonly SessionMessage[],
  transcriptPath: string
): { readonly role: string; readonly how: string } | { readonly detail: string } {
  const firstUser = messages.find((message) => message.role === "user");
  const text = firstUser === undefined ? "" : messageText(firstUser);
  const phase = CONTINUE_LINE.exec(text)?.[1];
  if (phase !== undefined) {
    const role = PHASE_ROLES[phase];
    return role === undefined
      ? { detail: `the phase ${phase} of its first user message has no role` }
      : {
          role,
          how: `the session's first user message starts the daemon's phase task "Continue … Phase: ${phase}." (workflow/effects.go), and ${phase} is worked by the ${role} (phase.go, RoleFor)`,
        };
  }
  const root = ROOT_START.exec(text)?.[1];
  if (root !== undefined) {
    return {
      role: "architect-root",
      how: `the session's first user message is the daemon's start of a tree's root architect, "${root} is yours and …" (workflow/architect_start.go)`,
    };
  }
  if (text.startsWith(CONTROLLER_START)) {
    return {
      role: "controller",
      how: `the session's first user message is the daemon's controller start, "${CONTROLLER_START}…" (daemon/controller.go)`,
    };
  }
  for (const message of messages) {
    if (message.role !== "custom" || message.customType !== "envoy-message") continue;
    const notice = messageText(message);
    if (!/^\s*from: agent$/m.test(notice)) continue;
    const tree = CATCH_UP_ROOT.exec(notice)?.[1];
    if (tree !== undefined) {
      return {
        role: "architect-root",
        how: `the daemon's catch-up notice to the session reads "the tree of ${tree} …", which the daemon writes to a tree's root architect (workflow/catch_up.go)`,
      };
    }
    const owned = CATCH_UP_CHILD.exec(notice)?.[1];
    if (owned !== undefined) {
      return {
        role: "architect",
        how: `the daemon's catch-up notice to the session reads "the issues under ${owned}, which you own …", which the daemon writes to the architect of an issue that is not a tree's root (workflow/catch_up.go)`,
      };
    }
  }
  const workspace = WORKSPACE_DIR.exec(transcriptPath);
  const workspaceIssue =
    workspace === null ? undefined : `${workspace[1]?.toUpperCase()}-${workspace[2]}`;
  for (const entry of branch) {
    if (entry.type !== "custom" || entry.customType !== "envoy-role-claim") continue;
    const token = RoleClaim.safeParse(entry).data?.data.role;
    if (token === undefined) continue;
    if (CONTROLLER_TOKEN.test(token)) {
      return {
        role: "controller",
        how: `the session claims the Envoy role ${token}, a project controller's claim token (claim/token.go, ControllerToken)`,
      };
    }
    const claimed = ROLE_TOKEN.exec(token);
    if (claimed === null) continue;
    const issue = `${claimed[1]?.toUpperCase()}-${claimed[2]}`;
    const role = claimed[3] ?? "";
    if (issue !== workspaceIssue) {
      return {
        detail: `it claims ${token}, and its path is no workspace of ${issue} (${workspaceIssue ?? "no Legion workspace"})`,
      };
    }
    if (role === "architect") {
      return {
        detail: `it claims ${token}, an architect's claim, and no message of the daemon says whether ${issue} is its tree's root`,
      };
    }
    return {
      role,
      how: `the session claims the Envoy role ${token}, the daemon's claim token of the ${role} of ${issue} (claim/token.go, NewToken), and its path is the workspace of ${issue}`,
    };
  }
  const firstCustom = messages.find((message) => message.role === "custom");
  const words = (text !== "" ? text : firstCustom === undefined ? "" : messageText(firstCustom))
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 80);
  return {
    detail: `no form names its role; its first ${text !== "" ? "user message" : "message"} starts: ${JSON.stringify(words)}`,
  };
}

// ---------------------------------------------------------------------------------------------
// Secrets

/** One secret shape: a pattern whose whole match, or the group `group` of it, is a secret. */
interface SecretShape {
  readonly name: string;
  readonly pattern: RegExp;
  /** The match less the secret: the text kept around it, with SECRET_REMOVED in its place. */
  readonly keep: (match: string, ...groups: string[]) => string;
}

const whole = () => SECRET_REMOVED;

/** The secret shapes the scan takes out, in order. */
const SECRET_SHAPES: readonly SecretShape[] = [
  {
    name: "private key",
    pattern: /-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY-----[\s\S]*?(?:-----END \1PRIVATE KEY-----|$)/g,
    keep: whole,
  },
  {
    name: "bearer",
    pattern: /\b(Bearer\s+)[A-Za-z0-9._~+/=-]{16,}/gi,
    keep: (_match, kept) => `${kept}${SECRET_REMOVED}`,
  },
  {
    name: "credential in a URL",
    pattern: /\b([a-z][a-z0-9+.-]*:\/\/[^\s:/@]+:)[^\s@/]+@/gi,
    keep: (_match, kept) => `${kept}${SECRET_REMOVED}@`,
  },
  {
    name: "token",
    pattern:
      /\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|xox[abprs]-[A-Za-z0-9-]{10,}|glpat-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|ya29\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})/g,
    keep: whole,
  },
  {
    name: "assignment",
    pattern:
      /\b([A-Za-z0-9_.-]*(?:api[_-]?key|apikey|secret|token|passwd|password|private[_-]?key|access[_-]?key)[A-Za-z0-9_.-]*["']?\s*[:=]\s*["']?)((?=[A-Za-z0-9_+/=.~-]*[0-9])(?=[A-Za-z0-9_+/=.~-]*[A-Za-z])[A-Za-z0-9_+/=.~-]{12,})/gi,
    keep: (_match, kept) => `${kept}${SECRET_REMOVED}`,
  },
];

/** `text` with each secret of SECRET_SHAPES taken out, and how many it took out. */
export function removeSecrets(text: string): { text: string; removed: number } {
  let removed = 0;
  let out = text;
  for (const shape of SECRET_SHAPES) {
    out = out.replace(shape.pattern, (match: string, ...groups: unknown[]) => {
      const kept = shape.keep(match, ...groups.filter((group) => typeof group === "string"));
      if (kept === match) return match;
      removed += 1;
      return kept;
    });
  }
  return { text: out, removed };
}

/** The fields whose strings the scan leaves alone: ids, and the signatures a replay must send
 *  back byte for byte. */
const UNSCANNED = new Set(["id", "toolCallId", "responseId", "thinkingSignature", "signature"]);

/** `value` with every secret of its strings taken out, and how many. */
function scrubbed<T>(value: T): { value: T; removed: number } {
  let removed = 0;
  const walk = (node: unknown): unknown => {
    if (typeof node === "string") {
      const result = removeSecrets(node);
      removed += result.removed;
      return result.text;
    }
    if (Array.isArray(node)) return node.map(walk);
    if (isObject(node)) {
      return Object.fromEntries(
        Object.entries(node).map(([key, each]) => [key, UNSCANNED.has(key) ? each : walk(each)])
      );
    }
    return node;
  };
  return { value: walk(value) as T, removed };
}

// ---------------------------------------------------------------------------------------------
// The profile

/** One part of a case's evidence. */
export interface Evidence {
  readonly item: string;
  readonly source: string;
}

/** The copies output, as the profile reads it (`readR4`): its prefix rows, installer rows and
 *  manifests, and each repository's default branch. */
interface Copies {
  readonly prefixes: readonly PrefixTableRow[];
  readonly installers: readonly InstallersRow[];
  readonly manifests: readonly ManifestsRow[];
  readonly branches: Map<string, string>;
}

/** The most GitHub reads of one repository's files in flight at once. */
const GITHUB_JOBS = 6;

// GitHub's answers, as copies.ts reads them: only the fields this file reads.
const GitTree = z.looseObject({
  tree: z.array(
    z.looseObject({
      path: z.string(),
      mode: z.string(),
      type: z.string(),
      sha: z.string(),
      size: z.number().optional(),
    })
  ),
  truncated: z.boolean().optional(),
});

/** What a repository holds at a commit, as copies.ts reads one: its skill copies, its manifest
 *  files parsed, and each file of those that could not be read or parsed, with why. */
interface RepositoryAt {
  readonly copies: readonly SkillCopy[];
  readonly manifests: readonly Manifest[];
  readonly faults: readonly { readonly file: string; readonly why: string }[];
}

/** The GitHub reads the profile makes, each made once. Exported for copies.test.ts, which checks
 *  that it reads a repository's files as copies.ts does. */
export class GithubReads {
  private readonly made = new Map<
    string,
    Promise<{ code: number; stdout: string; stderr: string }>
  >();
  private readonly held = new Map<string, Promise<RepositoryAt>>();
  private readonly blobs = new Map<string, Promise<string>>();

  constructor(
    private readonly gh: Gh,
    private readonly branches: ReadonlyMap<string, string>
  ) {}

  private call(args: readonly string[]) {
    const key = args.join("\0");
    let call = this.made.get(key);
    if (call === undefined) {
      call = this.gh(args);
      this.made.set(key, call);
    }
    return call;
  }

  private async ok(args: readonly string[]): Promise<string> {
    const result = await this.call(args);
    if (result.code !== 0) {
      const line = result.stderr.split("\n").find((each) => each.trim() !== "") ?? "";
      throw new Error(`gh ${args.join(" ")} failed: ${line}`);
    }
    return result.stdout;
  }

  /** The default branch of `repo`: repositories.tsv's, else GitHub's. */
  async branch(repo: string): Promise<{ branch: string; from: string }> {
    const known = this.branches.get(repo);
    if (known !== undefined && known !== "") return { branch: known, from: "repositories.tsv" };
    const branch = (await this.ok(["api", `repos/${repo}`, "--jq", ".default_branch"])).trim();
    if (branch === "") throw new Error(`GitHub names no default branch of ${repo}`);
    return { branch, from: `gh api repos/${repo}` };
  }

  /** The last commit of `branch` of `repo` at or before `at`. */
  async commitAt(repo: string, branch: string, at: string): Promise<string> {
    const sha = (
      await this.ok([
        "api",
        "-X",
        "GET",
        `repos/${repo}/commits`,
        "-f",
        `sha=${branch}`,
        "-f",
        `until=${at}`,
        "-f",
        "per_page=1",
        "--jq",
        ".[0].sha",
      ])
    ).trim();
    if (!/^[0-9a-f]{40}$/.test(sha))
      throw new Error(`no commit of ${repo} ${branch} at or before ${at}`);
    return sha;
  }

  /** The text of `file` of `repo` at `commit`, or undefined when it has none. */
  async text(repo: string, commit: string, file: string): Promise<string | undefined> {
    const args = [
      "api",
      "-X",
      "GET",
      `repos/${repo}/contents/${file.split("/").map(encodeURIComponent).join("/")}`,
      "-f",
      `ref=${commit}`,
      "-H",
      "Accept: application/vnd.github.raw",
    ];
    const result = await this.call(args);
    if (result.code === 0) return result.stdout;
    if (/\b404\b|Not Found/.test(result.stderr)) return undefined;
    return this.ok(args);
  }

  /** Every entry of the tree of `repo` at `commit`. The answer is not kept (a large repository's
   *  tree is megabytes); `at` keeps what it reads from it. Throws on a tree GitHub cuts short. */
  private async tree(repo: string, commit: string): Promise<TreeEntry[]> {
    const args = ["api", `repos/${repo}/git/trees/${commit}?recursive=1`];
    const result = await this.gh(args);
    if (result.code !== 0) {
      const line = result.stderr.split("\n").find((each) => each.trim() !== "") ?? "";
      throw new Error(`gh ${args.join(" ")} failed: ${line}`);
    }
    const tree = GitTree.safeParse(parseJson(result.stdout));
    if (!tree.success) throw new Error(`gh ${args.join(" ")} gave no git tree`);
    if (tree.data.truncated === true) {
      throw new Error(`GitHub cuts the tree of ${repo} at ${commit} short, so it is not read`);
    }
    return tree.data.tree;
  }

  /** The text of the blob `sha` of `repo`, read once: a blob's id is its content, so a file that
   *  two commits share is read for the first. */
  private blob(repo: string, sha: string): Promise<string> {
    let text = this.blobs.get(sha);
    if (text === undefined) {
      text = this.ok(["api", `repos/${repo}/git/blobs/${sha}`]).then((answer) => {
        const blob = GitBlob.safeParse(parseJson(answer));
        if (!blob.success || blob.data.encoding !== "base64") {
          throw new Error(`gh api repos/${repo}/git/blobs/${sha} gave no base64 blob`);
        }
        return Buffer.from(blob.data.content, "base64").toString("utf8");
      });
      this.blobs.set(sha, text);
    }
    return text;
  }

  /** Reads the blobs `entries` of `repo` that are not read yet, in GraphQL batches of up to
   *  BLOB_BATCH blobs and BLOB_BATCH_BYTES bytes (copies.ts's `blobBatches` and `blobQuery`), one
   *  call a batch where each blob alone would take one, with the owner and name as the fields
   *  `owner` and `name` (`graphqlArgs`), by which the pod's gh picks the owner's token. A blob a
   *  batch does not give whole (binary, cut short, missing, or in a batch that fails) is left to
   *  `blob`, which reads it alone. */
  private async prefetch(repo: string, entries: readonly TreeEntry[]): Promise<void> {
    const wanted = new Map<string, TreeEntry>();
    for (const entry of entries) {
      if (!this.blobs.has(entry.sha) && /^[0-9a-f]{40}$/.test(entry.sha)) {
        wanted.set(entry.sha, entry);
      }
    }
    for (const batch of blobBatches([...wanted.values()], BLOB_BATCH, BLOB_BATCH_BYTES)) {
      const result = await this.gh(graphqlArgs(repo, blobQuery(batch)));
      const parsed = BlobBatch.safeParse(result.code === 0 ? parseJson(result.stdout) : undefined);
      if (!parsed.success) continue;
      for (const [index, entry] of batch.entries()) {
        const blob = parsed.data.data.repository?.[`b${index}`];
        if (blob?.isBinary === false && !blob.isTruncated && blob.text !== null) {
          this.blobs.set(entry.sha, Promise.resolve(blob.text));
        }
      }
    }
  }

  /** What `repo` holds at `commit`, read as copies.ts's `listCopies` reads a repository (its
   *  steps 2 and 4): the files its rules select (`repositoryFiles`, of the tree's entries it keeps,
   *  `keepsEntry`), each `SKILL.md` as a skill copy and each manifest file parsed
   *  (`parseManifest`). Read once for each commit. */
  at(
    repo: string,
    commit: string,
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<RepositoryAt> {
    const key = `${repo}\0${commit}`;
    let held = this.held.get(key);
    if (held === undefined) {
      held = this.readAt(repo, commit, excludePaths);
      this.held.set(key, held);
    }
    return held;
  }

  private async readAt(
    repo: string,
    commit: string,
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<RepositoryAt> {
    const entries = await this.tree(repo, commit);
    const faults: { file: string; why: string }[] = [];
    const dirOf = (file: string) => {
      const dir = path.posix.dirname(file);
      return dir === "." ? "" : dir;
    };
    // No fork is read here: the entries kept are those of a repository that is no fork.
    const { skillFiles: skillEntries, manifests: candidates } = repositoryFiles(
      repo,
      entries.filter((entry) => keepsEntry(entry, false)),
      excludePaths
    );
    await this.prefetch(repo, [...skillEntries, ...candidates.map(({ entry }) => entry)]);
    const copies = await mapLimit(skillEntries, GITHUB_JOBS, async (entry): Promise<SkillCopy> => {
      const dir = dirOf(entry.path);
      const base = {
        repository: repo,
        path: dir,
        file: entry.path,
        blob: entry.sha,
        fork: false,
        archived: false,
        parent: "",
      };
      try {
        return { ...base, ...skillFields(dir, repo, await this.blob(repo, entry.sha)) };
      } catch (error) {
        const fault = `SKILL.md read failed: ${messageFor(error)}`;
        faults.push({ file: entry.path, why: fault });
        return {
          ...base,
          name: dir === "" ? (repo.split("/")[1] ?? "") : path.posix.basename(dir),
          nameFrom: "directory",
          description: "",
          descriptionBytes: 0,
          fault,
        };
      }
    });
    copies.sort((a, b) => a.path.localeCompare(b.path));
    const manifests = await mapLimit(
      candidates,
      GITHUB_JOBS,
      async ({ entry, kind }): Promise<Manifest> => {
        const base = { repository: repo, path: entry.path, kind, source: "tree" as const, rows: 0 };
        try {
          return {
            ...base,
            ...parseManifest(kind, entry.path, await this.blob(repo, entry.sha)),
            error: "",
          };
        } catch (error) {
          faults.push({ file: entry.path, why: `${kind}: ${messageFor(error)}` });
          return { ...base, facts: [], ofKind: true, says: "", error: messageFor(error) };
        }
      }
    );
    return { copies, manifests, faults };
  }
}

/** What the profile of one case is built from: its store, its context (the store's, for a
 *  subagent) and project, each successful read result of its messages whose first line is
 *  `[Skill file: <path>]` (any skill's), the session's start, its working directory, and whether
 *  its session recorded its own system prompt (a subagent's), which holds its context files. */
interface ProfileInput {
  readonly store: string;
  readonly context: string;
  readonly project: string | undefined;
  readonly reads: readonly ShownRead[];
  readonly sessionStart: string;
  readonly cwd: string | undefined;
  readonly recordedPrompt: boolean;
}

/** What every profile of one build reads. */
type ProfileRun = Pick<CaseRun, "config" | "copies" | "github" | "placer">;

/** The installers.tsv actions by which a unit's own manifest ships a skill. */
const MEMBER_ACTIONS: Readonly<Record<string, true>> = {
  ships: true,
  stages: true,
  links: true,
  installs: true,
};

/** A case's profile, the evidence for each part, and `shipped`: each copy some unit of the
 *  profile ships (`<repo>\0<name>`), whether or not a copy of its name came first. */
interface ProfileResult {
  readonly profile: Profile;
  readonly evidence: Evidence[];
  readonly shipped: ReadonlySet<string>;
}

/** Why a profile is refused: it installs no copy of the cut repository's skill. */
const notInstalled = (run: Pick<CaseRun, "skill" | "cut">) =>
  `the profile installs no ${run.skill} of ${run.cut.repo} at the session's start`;

/** The profile of a case, with the evidence for each part. Throws when a GitHub read fails. */
async function caseProfile(run: ProfileRun, input: ProfileInput): Promise<ProfileResult> {
  const { copies, github } = run;
  const evidence: Evidence[] = [];
  const commits = new Map<string, string>();
  /** The commit of `repo` at the session's start, and the evidence for it. */
  const readCommit = async (repo: string): Promise<{ sha: string; line: Evidence }> => {
    const { branch, from } = await github.branch(repo);
    const sha = await github.commitAt(repo, branch, input.sessionStart);
    return {
      sha,
      line: {
        item: `commit ${sha} of ${repo}`,
        source: `the last commit of ${branch} (from ${from}) at or before the session's start ${input.sessionStart}: gh api repos/${repo}/commits?sha=${branch}&until=${input.sessionStart}`,
      },
    };
  };
  const commitOf = async (repo: string): Promise<string> => {
    const known = commits.get(repo);
    if (known !== undefined) return known;
    const { sha, line } = await readCommit(repo);
    commits.set(repo, sha);
    evidence.push(line);
    return sha;
  };

  // Units by their `via` text: one unit id (`user-skills:omp`) can come from many manifests.
  const units = new Map<string, { via: Via; why: string }>();
  const viaKey = (via: Via) => `${via.unit} (${via.repository}:${via.manifest})`;
  // A copy a read is placed at that no prefix row names a unit for: it is installed alone.
  const alone = new Map<string, { repo: string; path: string; why: string }>();
  // The copy each skill name's first read shows, by attribute.ts's rules: the host gave the
  // session that copy of the name, so it is the profile's copy of it when a unit ships it then.
  const readCopies = new Map<string, { repo: string; path: string; index: number }>();
  const shown = new Set<string>();
  for (const read of input.reads) {
    if (read.skill !== undefined && !readCopies.has(read.skill)) {
      const copy = run.placer.place(input.store, read.header, read.skill).copy;
      if (copy !== undefined) {
        const [repo = "", dir = ""] = copy.split("\t");
        readCopies.set(read.skill, { repo, path: repoPath(dir), index: read.index });
      }
    }
    if (shown.has(read.header)) continue;
    shown.add(read.header);
    const reading = `message ${read.index} reads [Skill file: ${read.header}]`;
    let rows = copies.prefixes.filter((row) => prefixMatches(row.prefix ?? "", read.header));
    let under = (row: Record<string, string>) =>
      `${reading}, under the prefix-table.tsv row ${row.prefix}`;
    if (rows.length === 0) {
      // No prefix row maps the path, so attribute.ts's rules place it, as they placed the read in
      // the count: its units are those whose prefix rows name the copy it is placed at.
      const placed =
        read.skill === undefined
          ? undefined
          : run.placer.place(input.store, read.header, read.skill);
      if (placed?.copy === undefined) {
        evidence.push({
          item: `left out: the copy read at ${read.header}`,
          source: `message ${read.index}: no row of prefix-table.tsv maps that path${placed === undefined ? "" : `, and attribute.ts places it at no copy (${placed.outcome})`}`,
        });
        continue;
      }
      const [repository = "", dir = ""] = placed.copy.split("\t");
      const placing = `${reading}, which attribute.ts's ${placed.rule} rule places at ${repository} ${dir} (${placed.outcome})`;
      rows = copies.prefixes.filter((row) => row.repository === repository && row.path === dir);
      if (rows.length === 0) {
        alone.set(placed.copy, { repo: repository, path: repoPath(dir), why: placing });
        continue;
      }
      under = (row) =>
        `${placing}; the prefix-table.tsv row ${row.prefix} names a unit that ships that copy`;
    }
    for (const row of rows) {
      for (const via of vias(row.via ?? "")) {
        if (units.has(viaKey(via))) continue;
        units.set(viaKey(via), { via, why: under(row) });
      }
    }
  }
  if (input.context.startsWith("legion:")) {
    const ompUnits = new Set(
      copies.installers
        .filter((row) => row.host === "omp")
        .map((row) => `${row.unit} (${row.manifest_repository}:${row.manifest_path})`)
    );
    for (const row of copies.prefixes) {
      if (!(row.prefix ?? "").startsWith(IMAGE_ROOT)) continue;
      for (const via of vias(row.via ?? "")) {
        if (units.has(viaKey(via)) || !ompUnits.has(viaKey(via))) continue;
        units.set(viaKey(via), {
          via,
          why: `a Legion pod's worker image carries it: the prefix-table.tsv row ${row.prefix}`,
        });
      }
    }
    for (const row of copies.installers) {
      if (row.kind !== "omp-plugins") continue;
      const via = {
        unit: row.unit ?? "",
        repository: row.manifest_repository ?? "",
        manifest: row.manifest_path ?? "",
      };
      if (units.has(viaKey(via))) continue;
      if ((row.copy_path ?? "") === "") {
        evidence.push({
          item: `left out: the plugin ${row.unit}`,
          source: `installers.tsv: ${via.repository}:${via.manifest} installs it, and the copies output read no repository copy of it${row.note ? ` (${row.note})` : ""}`,
        });
        continue;
      }
      units.set(viaKey(via), {
        via,
        why: `installers.tsv: ${via.repository}:${via.manifest} installs it`,
      });
    }
  }

  // What each unit ships at the session's start. installers.tsv says what it ships at the commit
  // the copies output read, which is later; so each repository of a unit is read at its commit
  // of the session's start (its tree, its SKILL.md files and its manifests) and resolved by
  // copies.ts's own rules (`resolveInstallers`). A skill the unit did not ship yet is left out,
  // one it shipped then and not since is installed, and every skill of the profile is in its
  // repository at its commit.
  const listed = (via: Via) =>
    copies.installers.filter(
      (row) =>
        row.unit === via.unit &&
        row.manifest_repository === via.repository &&
        row.manifest_path === via.manifest &&
        (row.copy_path ?? "") !== "" &&
        Object.hasOwn(MEMBER_ACTIONS, row.action ?? "")
    );
  const repositories = new Set<string>();
  for (const { via } of units.values()) {
    repositories.add(via.repository);
    for (const row of listed(via)) repositories.add(row.copy_repository ?? "");
  }
  for (const copy of alone.values()) repositories.add(copy.repo);
  const held = new Map<string, RepositoryAt>();
  // The repositories' reads start together; their evidence, and the first failure, follow the
  // sorted order, as reading them one after another gives them.
  const reads = await Promise.allSettled(
    [...repositories].sort().map(async (repo) => {
      const commit = await readCommit(repo);
      return { repo, commit, at: await github.at(repo, commit.sha, run.config.excludePaths) };
    })
  );
  for (const settled of reads) {
    if (settled.status === "rejected") throw settled.reason;
    const { repo, commit, at } = settled.value;
    commits.set(repo, commit.sha);
    evidence.push(commit.line);
    held.set(repo, at);
    evidence.push({
      item: `the skills and manifests of ${repo} at ${commit.sha}`,
      source: `gh api repos/${repo}/git/trees/${commit.sha}?recursive=1: its ${at.copies.length} SKILL.md files and ${at.manifests.length} manifest files, read at that commit and resolved by copies.ts's rules (resolveInstallers), give what each unit ships then`,
    });
    for (const fault of at.faults) {
      evidence.push({
        item: `left out: ${repo} ${fault.file} at ${commit.sha}`,
        source: fault.why,
      });
    }
  }
  const resolved = resolveInstallers(
    [...held.values()].flatMap((at) => at.manifests),
    new CopyIndex([...held.values()].flatMap((at) => at.copies))
  );
  /** The rows by which `via`'s unit ships a skill at the session's start (copies.ts's rules). */
  const shipsAtStart = (via: Via) =>
    resolved.rows.filter(
      (row) =>
        row.unit === via.unit &&
        row.manifestRepository === via.repository &&
        row.manifestPath === via.manifest &&
        row.copyPath !== undefined &&
        Object.hasOwn(MEMBER_ACTIONS, row.action)
    );
  // A read whose skill no unit ships at the session's start, though the repository its unit comes
  // from was read at that commit: the copies output names the unit as the manifests name it at
  // the later commit it read, and a marketplace or a plugin renamed since leaves that unit empty
  // then (the read's path names the plugin cache directory `<marketplace>___<plugin>___<version>`
  // of the name it had).
  // The prefix rows the same manifests give at the session's start (copies.ts's rules,
  // `resolved.prefixes`) place the read at the unit that installed it then, and at its copy.
  const placedAtStart = new Set<string>();
  for (const read of input.reads) {
    const name = read.skill;
    if (name === undefined || placedAtStart.has(read.header)) continue;
    placedAtStart.add(read.header);
    if ([...units.values()].some(({ via }) => shipsAtStart(via).some((row) => row.skill === name)))
      continue;
    const rows = resolved.prefixes.filter(
      (row) => row.name === name && prefixMatches(row.prefix, read.header)
    );
    for (const row of rows) {
      for (const via of vias(row.via)) {
        if (units.has(viaKey(via))) continue;
        units.set(viaKey(via), {
          via,
          why: `message ${read.index} reads [Skill file: ${read.header}], under the prefix row ${row.prefix} that the manifests of ${via.repository} at ${commits.get(via.repository)} give by copies.ts's rules; no unit installers.tsv names for the read ships ${name} at that commit`,
        });
      }
    }
    const [row] = rows;
    const known = readCopies.get(name);
    if (row !== undefined && (known === undefined || known.index === read.index)) {
      readCopies.set(name, { repo: row.repository, path: row.path, index: read.index });
    }
  }

  const plugins = new Map<string, { repo: string; path: string; name: string; why: string }>();
  /** Each copy a unit ships at the session's start, in the order of the units, with how it ships
   *  and the evidence of it. */
  const offers: { copy: Omit<ProfileSkill, "commit">; how: string; source: string }[] = [];
  for (const [key, { via, why }] of units) {
    const id = via.unit;
    const start = `${via.repository} at ${commits.get(via.repository)}`;
    const members = shipsAtStart(via);
    const later = listed(via);
    evidence.push({ item: `unit ${key}`, source: why });
    if (members.length === 0) {
      evidence.push({
        item: `left out: the skills of ${key}`,
        source: `${start}: by copies.ts's rules ${via.manifest} ships no skill there`,
      });
    }
    for (const row of members) {
      const copy = { repo: row.copyRepository, path: row.copyPath ?? "", name: row.skill };
      const how = `${id} ${row.action} it (${row.manifestRepository}:${row.manifestPath})`;
      const inR4 = later.some(
        (each) => each.copy_repository === copy.repo && repoPath(each.copy_path ?? "") === copy.path
      );
      offers.push({
        copy,
        how,
        source: `${copy.repo} at ${commits.get(copy.repo)}: ${how}${inR4 ? "" : "; installers.tsv, which the copies output read later, does not list it"}`,
      });
    }
    for (const row of later) {
      const copy = { repo: row.copy_repository ?? "", path: repoPath(row.copy_path ?? "") };
      const shipped = members.some(
        (member) => member.copyRepository === copy.repo && member.copyPath === copy.path
      );
      if (shipped) continue;
      evidence.push({
        item: `left out: skill ${row.skill} (${copy.repo} ${copy.path})`,
        source: `installers.tsv, which the copies output read later, lists it for ${id}; ${id} does not ship it at the session's start (${start})`,
      });
    }
    const pluginOf = (): { repo: string; path: string; name: string } | undefined => {
      if (id.startsWith("package:")) {
        const manifest = resolved.rows.find((row) => row.unit === id && row.kind === "omp-package");
        if (manifest === undefined) return undefined;
        const dir = path.posix.dirname(manifest.manifestPath);
        return { repo: manifest.manifestRepository, path: dir, name: path.posix.basename(dir) };
      }
      if (id.startsWith("plugin:")) {
        const roots = new Set(
          members.map((row) => {
            const copyPath = row.copyPath ?? "";
            const tail = `/skills/${path.posix.basename(copyPath)}`;
            return copyPath.endsWith(tail)
              ? `${row.copyRepository}\0${copyPath.slice(0, -tail.length)}`
              : "";
          })
        );
        const [root] = roots;
        if (roots.size !== 1 || root === undefined || root === "") return undefined;
        const [repo = "", dir = ""] = root.split("\0");
        return { repo, path: dir, name: path.posix.basename(dir) };
      }
      return undefined;
    };
    const plugin = pluginOf();
    if (plugin === undefined) {
      if (!id.startsWith("user-skills:")) {
        evidence.push({
          item: `left out: a plugin directory of ${id}`,
          source: `${start}: no Oh My Pi package manifest ships it there, and its skills share no plugin directory`,
        });
      }
    } else if (!plugins.has(plugin.name)) {
      plugins.set(plugin.name, { ...plugin, why: `the directory of ${id}` });
    }
  }
  for (const copy of alone.values()) {
    const start = `${copy.repo} at ${commits.get(copy.repo)}`;
    const found = held.get(copy.repo)?.copies.find((each) => each.path === copy.path);
    if (found === undefined) {
      evidence.push({
        item: `left out: the copy ${copy.repo} ${copy.path}`,
        source: `${copy.why}; ${start} holds no SKILL.md there`,
      });
      continue;
    }
    const how = `${copy.why}; no row of prefix-table.tsv names a unit that ships that copy, so the profile installs it alone`;
    offers.push({
      copy: { repo: copy.repo, path: copy.path, name: found.name },
      how,
      source: `${start}: ${how}`,
    });
  }

  // Each name's copy: the one the session read, when a unit ships it at the session's start (the
  // read shows which of the copies of one name the host gave the session), else the first unit's.
  const same = (a: { repo: string; path: string }, b: { repo: string; path: string }) =>
    a.repo === b.repo && a.path === b.path;
  const chosen = new Map<string, (typeof offers)[number]>();
  for (const [name, read] of readCopies) {
    const offer = offers.find((each) => each.copy.name === name && same(each.copy, read));
    if (offer !== undefined) chosen.set(name, offer);
  }
  for (const offer of offers) {
    if (!chosen.has(offer.copy.name)) chosen.set(offer.copy.name, offer);
  }
  const skills = new Map<string, Omit<ProfileSkill, "commit">>();
  for (const offer of offers) {
    const pick = chosen.get(offer.copy.name);
    if (pick === undefined) continue;
    const read = readCopies.get(offer.copy.name);
    const readAt = read !== undefined && same(pick.copy, read) ? read.index : undefined;
    if (pick === offer) {
      skills.set(offer.copy.name, offer.copy);
      evidence.push({
        item: `skill ${offer.copy.name} (${offer.copy.repo} ${offer.copy.path})`,
        source: `${offer.source}${readAt === undefined ? "" : `; message ${readAt} reads this copy`}`,
      });
    } else if (!same(pick.copy, offer.copy)) {
      evidence.push({
        item: `left out: skill ${offer.copy.name} from ${offer.copy.repo} ${offer.copy.path}`,
        source: `${offer.how} too; ${readAt === undefined ? `the copy of ${pick.copy.repo} ${pick.copy.path} came first` : `the session read the copy of ${pick.copy.repo} ${pick.copy.path} (message ${readAt})`}`,
      });
    }
  }
  const shipped = new Set(offers.map((offer) => `${offer.copy.repo}\0${offer.copy.name}`));

  // Context files: the session repository's AGENTS.md and the project's instructions file, which
  // a recorded prompt already holds as its session had them; for a devbox, whose working directory
  // names no repository, the user-level context file Oh My Pi reads. `user` says why a file is
  // that one.
  const contextFiles: { repo: string; path: string; user?: string }[] = [];
  const legion = input.context.startsWith("legion:");
  if (input.recordedPrompt) {
    evidence.push({
      item: "no context files",
      source:
        "the session's recorded system prompt holds the context files it was given, and the capture sends that prompt",
    });
  } else if (legion) {
    const workspace = LEGION_WORKSPACE.exec(input.cwd ?? "");
    if (workspace === null) {
      evidence.push({
        item: NO_WORKSPACE,
        source: `the session's working directory ${input.cwd ?? "(none)"} is no Legion workspace /legion/workspaces/<owner>/<name>/`,
      });
    } else {
      contextFiles.push({ repo: `${workspace[1]}/${workspace[2]}`, path: "AGENTS.md" });
    }
    const project = copies.manifests.find(
      (row) =>
        row.kind === "legion-project" && (row.says ?? "").startsWith(`project ${input.project}:`)
    );
    if (project === undefined) {
      evidence.push({
        item: `left out: the instructions file of the project ${input.project}`,
        source:
          "manifests.tsv of the copies output holds no legion-project manifest of that project",
      });
    } else {
      contextFiles.push({ repo: project.repository ?? "", path: `project:${project.path ?? ""}` });
    }
  } else {
    evidence.push({
      item: "left out: the working directory's context files",
      source: "a devbox session's working directory names no repository",
    });
    // Oh My Pi's one user-level context file (discovery/claude.ts at the pinned tag) is CLAUDE.md
    // in Claude Code's config directory, which a dev box's CLAUDE_CONFIG_DIR names: the `.claude/`
    // of the repository that installs its units, which holds Claude Code's settings there.
    const unitRepos = [...new Set([...units.values()].map(({ via }) => via.repository))].sort();
    const homes = unitRepos.filter((repo) =>
      held
        .get(repo)
        ?.manifests.some((each) => each.kind === "claude-settings" && each.path === CLAUDE_SETTINGS)
    );
    const [home] = homes;
    if (homes.length === 1 && home !== undefined) {
      contextFiles.push({
        repo: home,
        path: USER_CONTEXT_FILE,
        user: `Oh My Pi's user-level context file is CLAUDE.md in Claude Code's config directory, on a dev box the .claude/ of the repository that installs its units: of the repositories of the profile's units (${unitRepos.join(", ")}), ${home} alone holds Claude Code's settings ${CLAUDE_SETTINGS} at ${commits.get(home)}`,
      });
    } else {
      evidence.push({
        item: "left out: Oh My Pi's user-level context file",
        source: `${homes.length === 0 ? "no repository" : `${homes.length} repositories (${homes.join(", ")})`} of the profile's units (${unitRepos.join(", ") || "none"}) ${homes.length === 0 ? "holds" : "hold"} Claude Code's settings ${CLAUDE_SETTINGS} at the session's start, so which is the dev box's Claude Code config directory, whose CLAUDE.md Oh My Pi reads at user level, is not known`,
      });
    }
  }

  const profileSkills: ProfileSkill[] = [];
  for (const skill of [...skills.values()].sort((a, b) =>
    a.name < b.name ? -1 : a.name > b.name ? 1 : 0
  )) {
    profileSkills.push({ ...skill, commit: await commitOf(skill.repo) });
  }
  const profilePlugins: Profile["plugins"] = [];
  for (const plugin of plugins.values()) {
    const commit = await commitOf(plugin.repo);
    profilePlugins.push({ repo: plugin.repo, commit, path: plugin.path, name: plugin.name });
    evidence.push({
      item: `plugin ${plugin.name} (${plugin.repo} ${plugin.path})`,
      source: plugin.why,
    });
  }
  const profileFiles: Profile["contextFiles"] = [];
  for (const file of contextFiles) {
    const commit = await commitOf(file.repo);
    if (file.path.startsWith("project:")) {
      const projectFile = file.path.slice("project:".length);
      const text = await github.text(file.repo, commit, projectFile);
      const fields = text === undefined ? undefined : (Bun.YAML.parse(text) as unknown);
      const instructions = isObject(fields) ? fields.instructions : undefined;
      if (typeof instructions !== "string" || instructions === "") {
        evidence.push({
          item: `left out: the instructions file of the project ${input.project}`,
          source: `${file.repo} ${projectFile} at ${commit} names no instructions file`,
        });
        continue;
      }
      const instructionsPath = path.posix.join(
        path.posix.dirname(path.posix.dirname(projectFile)),
        "instructions",
        instructions
      );
      profileFiles.push({ repo: file.repo, commit, path: instructionsPath });
      evidence.push({
        item: `context file ${file.repo} ${instructionsPath}`,
        source: `the project file ${projectFile} (manifests.tsv) says instructions: ${instructions}`,
      });
      continue;
    }
    if ((await github.text(file.repo, commit, file.path)) === undefined) {
      evidence.push({
        item: `left out: ${file.repo} ${file.path}`,
        source:
          file.user === undefined
            ? `the repository of the session's working directory ${input.cwd} has no ${file.path} at ${commit}`
            : `${file.user}, and it has no ${file.path} at ${commit}`,
      });
      continue;
    }
    profileFiles.push({ repo: file.repo, commit, path: file.path });
    evidence.push({
      item: `context file ${file.repo} ${file.path}`,
      source:
        file.user === undefined
          ? `the session's working directory ${input.cwd} is a workspace of ${file.repo}, which has ${file.path} at ${commit}`
          : `${file.user}, and it has ${file.path} at ${commit}`,
    });
  }
  return {
    profile: { skills: profileSkills, plugins: profilePlugins, contextFiles: profileFiles },
    evidence,
    shipped,
  };
}

// ---------------------------------------------------------------------------------------------
// build-cases

const BUILD_CASES_USAGE =
  "usage: build.ts build-cases --config <skill-cuts.json> --key-file <file> --cut <id> --skill <name> --old <dir> --sessions <file> --seed <n> --copies <dir> --count <dir> --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]\n       build.ts build-cases --config <skill-cuts.json> --old <dir> --copies <dir> --count <dir> --reprofile <cases.jsonl> --out <new cases.jsonl>";
const BUILD_TASKS_USAGE =
  "usage: build.ts build-tasks --config <skill-cuts.json> --key-file <file> --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir> --context-session <store>:<hash>] [--max-candidates <n>] [--max-search-minutes <m>]\n       build.ts build-tasks --first-read --config <skill-cuts.json> --key-file <file> --skill <dir> --sessions <sessions.jsonl> --seed <n> [--max <n>] --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir>]";

/** One candidate session of the sessions file. */
const Candidate = z.strictObject({
  store: z.string().min(1),
  hash: z.string().regex(/^[0-9a-f]{16}$/, "is not 16 hex characters"),
  path: z.string().min(1).optional(),
});
type Candidate = z.infer<typeof Candidate>;

/** A refusal unless `file` is a file. */
function mustBeFile(file: string, what: string): void {
  if (!statSync(file, { throwIfNoEntry: false })?.isFile())
    throw new Refused(`${what} ${file} is not a file`);
}

/** The skill in `dir` as build-tasks reads it: its SKILL.md's frontmatter name (its directory's
 *  name when it gives none) and description. Refuses a directory with no SKILL.md. */
function taskSkill(dir: string): { name: string; description: string } {
  const skillFile = path.join(dir, "SKILL.md");
  mustBeFile(skillFile, "--skill's SKILL.md");
  const fields = frontmatter(readFileSync(skillFile, "utf8")) ?? {};
  return {
    name:
      typeof fields.name === "string" && fields.name !== ""
        ? fields.name
        : path.basename(path.resolve(dir)),
    description: typeof fields.description === "string" ? fields.description : "",
  };
}

/** The gateway of a builder's readers, which starts no call once its spend reaches `budgetUsd`,
 *  and takes `concurrency` calls at once. */
function readerGateway(
  config: SkillCutsConfig,
  keyFile: string,
  budgetUsd?: number,
  concurrency = 1
): Gateway {
  return createGateway({
    baseUrl: config.gatewayBaseUrl,
    keyFile,
    concurrency,
    timeoutMs: READER_TIMEOUT_MS,
    ...(budgetUsd === undefined ? {} : { budgetUsd }),
  });
}

/** A session no longer wanted: the build ended while it was open, so it makes no further call. */
class NotNeeded extends Error {}

/** The candidates of the sessions file `file`, each once. Throws a refusal naming each line at
 *  fault: not JSON, not a candidate, a store the configuration does not name, or a path whose hash
 *  is not its `hash`. */
function readCandidates(file: string, config: SkillCutsConfig): Candidate[] {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`cannot read --sessions ${file}: ${messageFor(error)}`);
  }
  const faults: string[] = [];
  const seen = new Set<string>();
  const candidates: Candidate[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    const where = `${file}:${index + 1}`;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch (error) {
      faults.push(`${where} is not JSON: ${messageFor(error)}`);
      continue;
    }
    const parsed = Candidate.safeParse(value);
    if (!parsed.success) {
      faults.push(
        `${where} is not {"store", "hash", "path"?}: ${schemaFaults(parsed.error, "the line")}`
      );
      continue;
    }
    const candidate = parsed.data;
    if (!config.stores.some((store) => store.name === candidate.store)) {
      faults.push(
        `${where} names the store ${candidate.store}, which the configuration does not name`
      );
      continue;
    }
    if (candidate.path !== undefined && fileHash(candidate.path) !== candidate.hash) {
      faults.push(
        `${where}: its path's hash is ${fileHash(candidate.path)}, not ${candidate.hash}`
      );
      continue;
    }
    const key = `${candidate.store}\0${candidate.hash}`;
    if (seen.has(key)) continue;
    seen.add(key);
    candidates.push(candidate);
  }
  if (faults.length > 0) throw new Refused(`refused --sessions lines:\n${faults.join("\n")}`);
  return candidates;
}

/** What one session gave: a case, or a drop with its reason. */
type Outcome =
  | { readonly case: Case; readonly step: number }
  | { readonly drop: DropReason; readonly detail: string };

/** What one session's build reads beside the session. */
interface CaseRun {
  readonly config: SkillCutsConfig;
  readonly cut: { readonly id: string; readonly repo: string };
  readonly skill: string;
  readonly old: string;
  /** The skill's base files (`--old`). */
  readonly files: readonly { readonly path: string; readonly text: string }[];
  readonly copies: Copies;
  readonly github: GithubReads;
  readonly gateway: Gateway;
  /** attribute.ts's placing of reads, for the build's `--count` and `--copies`. */
  readonly placer: Placer;
  /** The ids (attribute.ts's `modelId`) of the configuration's `servedModels`. */
  readonly servedIds: ReadonlySet<string>;
}

/** One session's record in the summary. `model`: the session's model as its last `model_change`
 *  names it, before the step once a step is found, else before the skill's first successful read
 *  (absent when there is none). `windows`: how many windows of the session the case reader read.
 *  `restatementCheck`, once a step is found and the case's messages are known: how many of the
 *  messages before the step the restatement check must read (the case's messages less the skill's
 *  own read results), how many it read whole, and in how many windows; a case is kept only when
 *  `read` is `before`. `audit`: the audit's verdict on the real step and its reason. */
interface SessionRecord {
  store: string;
  hash: string;
  pathHash?: string;
  pulls?: number;
  readerUsd: number;
  outcome: string;
  reason?: DropReason;
  detail?: string;
  context?: string;
  model?: string;
  windows?: number;
  restatementCheck?: { read: number; before: number; windows: number };
  audit?: { verdict: string; reason: string };
}

/** A store as a session's context reads it: its name and its context. */
interface StoreContext {
  readonly name: string;
  readonly context: string;
}

/**
 * A session's context as build-cases rebuilds it: a subagent's (`subagent:<agent>`), from its
 * recorded `session_init`; else, for a Legion store, the store's context and the session's role
 * (`sessionRole`, the module comment's "The role"); else the store's context. With the evidence of
 * it, the Legion project and the `session_init` line; or why it has none.
 */
function sessionContext(
  store: StoreContext,
  t: Transcript,
  transcriptPath: string
):
  | {
      readonly context: string;
      readonly project: string | undefined;
      readonly evidence: Evidence[];
      readonly init: SessionInit | undefined;
    }
  | { readonly drop: DropReason; readonly detail: string } {
  const project = store.context.startsWith("legion:")
    ? store.context.slice("legion:".length)
    : undefined;
  const evidence: Evidence[] = [];
  const init = sessionInit(t.entries);
  if (init === null) return { drop: "bad-session-init", detail: DROP_REASONS["bad-session-init"] };
  if (init !== undefined) {
    evidence.push(
      {
        item: `subagent ${init.agent}`,
        source: `the session's session_init line names the agent ${init.agent}; its parent session is another transcript, and the store's context is ${store.context}`,
      },
      {
        item: "the recorded system prompt and tools",
        source: `the session's session_init line records the system prompt it was given (${init.systemPrompt.length} blocks) and its ${init.tools.length} tools (${init.tools.join(", ")}); the case carries them, and the capture sends that prompt with only the skill's listing line changed per arm, and only those tools`,
      }
    );
    return { context: `subagent:${init.agent}`, project, evidence, init };
  }
  if (project !== undefined) {
    // sessionRole reads the text of the first user message, of the first custom message and of
    // each Envoy message; those are read whole, the rest stay light.
    const all = t.items.map((item) => item.slim);
    const firstUser = all.findIndex((message) => message.role === "user");
    const firstCustom = all.findIndex((message) => message.role === "custom");
    const roleMessages = all.map((message, index) =>
      index === firstUser ||
      index === firstCustom ||
      (message.role === "custom" && message.customType === "envoy-message")
        ? t.message(index)
        : message
    );
    const found = sessionRole(t.branch, roleMessages, transcriptPath);
    if ("detail" in found) return { drop: "no-role", detail: found.detail };
    evidence.push({ item: `role ${found.role}`, source: found.how });
    return { context: `${store.context}:${found.role}`, project, evidence, init };
  }
  evidence.push({ item: "no role", source: `the store's context is ${store.context}` });
  return { context: store.context, project, evidence, init };
}

/** A session's start and working directory, from its header, or why it has none. */
function sessionHeader(
  t: Transcript
):
  | { readonly sessionStart: string; readonly cwd: string | undefined }
  | { readonly drop: DropReason; readonly detail: string } {
  const header = t.entries.find((entry) => entry.type === "session");
  const sessionStart = header?.timestamp;
  if (
    sessionStart === undefined ||
    !z.iso.datetime({ offset: true }).safeParse(sessionStart).success
  ) {
    return { drop: "no-header", detail: "no session line with an ISO timestamp" };
  }
  return { sessionStart, cwd: typeof header?.cwd === "string" ? header.cwd : undefined };
}

/** A load test's profile of one session (load.ts's LoadProfile: cases.ts's Profile and the
 *  context), the evidence for each part, the secrets taken out of it, and whether a load test of
 *  the skill in it lists the skill (`listsSkill`). */
export interface SessionProfile {
  readonly profile: Profile & { readonly context: string };
  readonly evidence: Evidence[];
  readonly secretsRemoved: number;
  readonly listed: boolean;
}

/**
 * Whether a load test of `skill` in the context `profile` gives lists the skill, so its first reply
 * can load it. A recorded system prompt (a subagent's) is what the capture sends, with only the
 * skill's line of its listing changed: it lists the skill when capture.ts's `armPromptBlocks`
 * finds that line, and load refuses the task (`its capture does not list`) when it does not. Any
 * other context lists the skill when the session's own profile installs it, as one of its skills
 * (a plugin's or a package's skills among them). `skills` is the rebuilt profile's skills before
 * the tested one is left out.
 */
function listsSkill(
  profile: Pick<Profile, "systemPrompt">,
  skills: readonly ProfileSkill[],
  skill: string
): boolean {
  if (profile.systemPrompt !== undefined) {
    return armPromptBlocks(profile.systemPrompt, skill, undefined).line !== "not listed";
  }
  return skills.some((each) => each.name === skill);
}

/**
 * The load test's profile of the session whose transcript is `t` (read from the file
 * `transcriptPath` of `store`; its caller parsed it, `readTranscript`), rebuilt as build-cases
 * rebuilds a case's at the session's start: its context (`sessionContext`), and `caseProfile` from
 * every read of its branch, so the units its host had, at their commits then, their plugins and
 * the context files; a subagent's recorded system prompt (its secrets taken out) and tools. The
 * tested skill `skill` is left out of the skills, since the load test installs it. Throws, naming
 * why, when the session gives none.
 */
async function sessionProfile(
  run: ProfileRun,
  store: StoreContext,
  transcriptPath: string,
  t: Transcript,
  skill: string
): Promise<SessionProfile> {
  const header = sessionHeader(t);
  if ("drop" in header) throw new Error(`${header.drop}: ${header.detail}`);
  const rebuilt = sessionContext(store, t, transcriptPath);
  if ("drop" in rebuilt) throw new Error(`${rebuilt.drop}: ${rebuilt.detail}`);
  const { context, project, evidence, init } = rebuilt;
  const profiled = await caseProfile(run, {
    store: store.name,
    context: store.context,
    project,
    sessionStart: header.sessionStart,
    cwd: header.cwd,
    recordedPrompt: init !== undefined,
    reads: shownReads(t.items.map((item) => item.slim)),
  });
  const secrets = scrubbed({ systemPrompt: init?.systemPrompt });
  const recorded =
    init === undefined || secrets.value.systemPrompt === undefined
      ? {}
      : { systemPrompt: secrets.value.systemPrompt, tools: init.tools };
  return {
    profile: {
      ...profiled.profile,
      ...recorded,
      skills: profiled.profile.skills.filter((each) => each.name !== skill),
      context,
    },
    evidence: [...evidence, ...profiled.evidence],
    secretsRemoved: secrets.removed,
    listed: listsSkill(recorded, profiled.profile.skills, skill),
  };
}

/** The case of one session's transcript, pulled into the file `spool`, or why it gives none.
 *  Adds what the reader spent and what its restatement check read to `record`. Throws NotNeeded
 *  before a reader call once `session.stopped` is set. The transcript is read from the spool
 *  line by line, and a message is read whole only where a check or a reader needs it. */
async function caseOf(
  run: CaseRun,
  store: { readonly name: string; readonly context: string },
  transcriptPath: string,
  spool: string,
  record: SessionRecord,
  session: { readonly stopped: boolean }
): Promise<Outcome> {
  const drop = (reason: DropReason, detail: string): Outcome => ({ drop: reason, detail });
  let t: Transcript;
  try {
    t = readTranscript(spool);
  } catch (error) {
    return drop("unparseable", messageFor(error));
  }
  const header = sessionHeader(t);
  if ("drop" in header) return drop(header.drop, header.detail);
  const { sessionStart } = header;
  // Every message, light: what the scans of the whole session read.
  const allMessages = t.items.map((item) => item.slim);
  const reads = skillReads(allMessages, run.skill);
  const first = reads.find((read) => read.ok);
  if (first === undefined) {
    return drop(
      "no-read",
      `${reads.length} read results of skill://${run.skill} files, none successful`
    );
  }
  const firstAt = t.items[first.index]?.at ?? 0;
  record.model = lastBefore(t.branch, firstAt, modelChange);
  // The copy the session read, placed by attribute.ts's own rules from the same count and copies
  // output as the sessions file, so the builder keeps what attribute.ts counted for the cut's
  // copy: a read in a checkout or one the store's context places, as well as one the prefix table
  // maps.
  const placed = run.placer.place(store.name, first.header ?? "", run.skill);
  const [placedRepository = "", placedPath = ""] = (placed.copy ?? "").split("\t");
  if (placed.copy === undefined) {
    return drop("not-cut-copy", `no rule places ${first.header} at a copy: ${placed.outcome}`);
  }
  if (placedRepository !== run.cut.repo) {
    return drop(
      "not-cut-copy",
      `the ${placed.rule} rule places ${first.header} at ${placedRepository} ${placedPath}${placed.outcome === undefined ? "" : ` (${placed.outcome})`}`
    );
  }

  // The model and thinking level: the step's are the last before it, and the step comes after the
  // read, so each is the one in effect at the read or one set after it. A session that gives the
  // step no model, no thinking level Oh My Pi takes, or no model the gateway serves, whichever
  // step the reader finds, gives no case, and no reader reads it; the step's own checks, below,
  // would drop it for the same reason.
  const models = new Set(record.model === undefined ? [] : [record.model]);
  const levels = [lastBefore(t.branch, firstAt, thinkingChange)];
  for (const entry of t.branch.slice(firstAt + 1)) {
    const model = modelChange(entry);
    if (model !== undefined) models.add(model);
    const level = thinkingChange(entry);
    if (level !== undefined) levels.push(level);
  }
  if (models.size === 0) return drop("no-model", "no model_change");
  if (
    record.model !== undefined &&
    !levels.some((level) => level !== undefined && Thinking.safeParse(level).success)
  ) {
    return drop("no-model", "no thinking level Oh My Pi takes");
  }
  if (![...models].some((model) => served(model, run.servedIds).served)) {
    return drop("model-not-served", notServed([...models], run.servedIds));
  }

  // The context: a subagent's, from its recorded session_init; else, for a Legion store, the
  // session's role (the module comment, "The role").
  const rebuilt = sessionContext(store, t, transcriptPath);
  if ("drop" in rebuilt) return drop(rebuilt.drop, rebuilt.detail);
  const { context, project, evidence, init } = rebuilt;
  record.context = context;

  // The case reader, window by window, from the read's call on: each window at most
  // READER_MESSAGES messages and READER_CHARS characters, and, for its search for a restatement
  // of the line, before it at most READER_EARLIER_MESSAGES messages and READER_EARLIER_CHARS
  // characters of those the model still read there (after the last compaction or context reset
  // before the window), nearest first. The next window starts after the last message of the one
  // before, until the reader proposes a step, READER_WINDOWS windows are read, or the messages
  // run out. No window reads past the first compaction or context reset after the skill's read,
  // since a step after it is dropped (`read-before-compaction`) whatever the reader says.
  const from = Math.min(first.call, first.index);
  const boundaryAfterRead = t.branch.findIndex(
    (entry, at) => at > firstAt && (entry.type === "compaction" || entry.type === "reset_boundary")
  );
  const readable = t.items.filter((item) => boundaryAfterRead < 0 || item.at < boundaryAfterRead);

  // The lines the case reader may name: those of the base text the session's own successful
  // reads of the skill's files held, among the messages it reads. A session with none gives no
  // case, and no reader reads it.
  const readsInReach = reads.filter((read) => read.ok && read.index < readable.length);
  const offered = offeredFiles(run.files, readsInReach, t.message);
  if (offered.length === 0) {
    const read = [...new Set(readsInReach.map((each) => each.file))];
    return drop(
      "no-common-line",
      `the session's successful reads of ${read.join(", ")} hold no whole line of the base text`
    );
  }

  // The profile at the session's start, before any reader reads the session. The case's profile
  // is built from the reads before its step, which the reader has not found yet; those are among
  // the reads the reader can reach, and fewer reads install fewer units. So when no unit of the
  // reads it can reach ships the skill of the cut's repository, no step's profile installs it, and
  // the session is dropped as the step's profile check would drop it. A GitHub read that fails
  // here is the step's read too.
  const profileInput = {
    store: store.name,
    context: store.context,
    project,
    sessionStart,
    cwd: header.cwd,
    recordedPrompt: init !== undefined,
  };
  let reachable: ProfileResult;
  try {
    reachable = await caseProfile(run, {
      ...profileInput,
      reads: shownReads(readable.map((item) => item.slim)),
    });
  } catch (error) {
    return drop("profile-failed", messageFor(error));
  }
  if (!reachable.shipped.has(`${run.cut.repo}\0${run.skill}`)) {
    return drop("not-cut-copy", notInstalled(run));
  }
  const hidden = skillReadResults(allMessages, run.skill);
  const shown = (index: number, message: SessionMessage): CaseInput["messages"][number] => ({
    index,
    role: message.role ?? "message",
    text: hidden.has(index)
      ? "[tool result: read]\n[the text of a file of the skill, left out: <skill> shows the lines of it a case may name]"
      : cut(
          typeof message.summary === "string"
            ? `${renderMessage(message)}\n${message.summary}`
            : renderMessage(message),
          MESSAGE_CHARS
        ),
  });
  let answer: Exclude<CaseAnswer, { step: null }> | undefined;
  let noStep = "";
  let start = from;
  let windows = 0;
  while (answer === undefined && windows < READER_WINDOWS && start < readable.length) {
    windows += 1;
    record.windows = windows;
    const windowMessages: CaseInput["messages"] = [];
    let chars = 0;
    for (
      let index = start;
      index < readable.length && windowMessages.length < READER_MESSAGES;
      index += 1
    ) {
      const item = readable[index];
      if (item === undefined) break;
      const each = shown(index, t.message(index));
      if (windowMessages.length > 0 && chars + each.text.length > READER_CHARS) break;
      chars += each.text.length;
      windowMessages.push(each);
    }
    const startAt = t.items[start]?.at ?? 0;
    const boundary = t.branch.findLastIndex(
      (entry, at) =>
        at < startAt && (entry.type === "compaction" || entry.type === "reset_boundary")
    );
    const earlier: CaseInput["messages"] = [];
    let earlierChars = 0;
    for (
      let index = start - 1;
      index >= 0 && earlier.length < READER_EARLIER_MESSAGES;
      index -= 1
    ) {
      const item = t.items[index];
      if (item === undefined || item.at <= boundary) break;
      const each = shown(index, t.message(index));
      if (earlier.length > 0 && earlierChars + each.text.length > READER_EARLIER_CHARS) break;
      earlierChars += each.text.length;
      earlier.unshift(each);
    }
    let given: CaseAnswer;
    if (session.stopped) throw new NotNeeded();
    try {
      const result = await runReader(
        "case",
        { skill: run.skill, files: offered, messages: [...earlier, ...windowMessages] },
        run.gateway
      );
      record.readerUsd += usdOf(result.spent);
      given = CaseAnswer.parse(result.answer);
    } catch (error) {
      if (error instanceof BudgetSpent) throw error;
      if (error instanceof ReaderFailed) {
        record.readerUsd += usdOf(error.spent);
        return drop("reader-failed", error.message);
      }
      return drop("reader-error", messageFor(error));
    }
    if (given.step === null) noStep = given.reason;
    else answer = given;
    start = (windowMessages.at(-1)?.index ?? start) + 1;
  }
  if (answer === undefined) {
    const extent = `${windows} window${windows === 1 ? "" : "s"} read, to message ${start - 1} of ${allMessages.length}`;
    if (boundaryAfterRead >= 0 && start >= readable.length) {
      return drop(
        "read-before-compaction",
        `the case reader found no step before the ${t.branch[boundaryAfterRead]?.type} after the skill's read, and a step after it would follow a skill read that precedes it (${extent}): ${noStep}`
      );
    }
    return drop("no-step", `${noStep} (${extent})`);
  }
  const stepIndex = answer.step.messageIndex;
  if (stepIndex <= first.index) {
    return drop("step-not-after-read", `step ${stepIndex}, read result ${first.index}`);
  }
  const stepItem = t.items[stepIndex];
  if (stepItem === undefined || stepItem.slim.role !== "assistant") {
    return drop("no-step", `message ${stepIndex} is no assistant message`);
  }

  // The model and thinking level before the step. The case keeps the model as the session names
  // it, and runs on no other.
  const model = lastBefore(t.branch, stepItem.at, modelChange);
  if (model !== undefined) record.model = model;
  const thinking = Thinking.safeParse(lastBefore(t.branch, stepItem.at, thinkingChange));
  if (model === undefined || !thinking.success) {
    return drop(
      "no-model",
      model === undefined ? "no model_change" : "no thinking level Oh My Pi takes"
    );
  }
  if (!served(model, run.servedIds).served) {
    return drop("model-not-served", notServed([model], run.servedIds));
  }

  // The messages before the step, and the checks of the line against them.
  const before = messagesBefore(t, stepItem.at, firstAt);
  if ("drop" in before) return drop(before.drop, before.detail);
  const messages = before.messages;
  const caseReads = skillReads(messages, run.skill);
  const installedDirs = caseReads.flatMap((read) =>
    read.header?.endsWith(`/${read.file}`) ? [read.header.slice(0, -(read.file.length + 1))] : []
  );
  const byPath = readByPath(messages, run.skill, installedDirs);
  if (byPath !== undefined) return drop("read-by-path", byPath);

  const file = run.files.find((each) => each.path === answer.line.file);
  const wanted = answer.line.text.trim();
  const baseLines = file === undefined ? [] : textLines(file.text);
  const lineNumber = baseLines.findIndex((line) => line.trim() === wanted) + 1;
  if (file === undefined || lineNumber === 0) {
    return drop("line-not-in-base", `${answer.line.file}: ${JSON.stringify(answer.line.text)}`);
  }
  const held = caseReads.some((read) => {
    const message = messages[read.index];
    return (
      read.ok &&
      read.file === file.path &&
      textLines(stripSkillHeader(message?.content?.[0]?.text ?? "")).some(
        (line) => line.trim() === wanted
      )
    );
  });
  if (!held) {
    return drop(
      "line-not-in-read",
      `no successful read result of ${file.path} before message ${stepIndex} holds ${JSON.stringify(wanted)}`
    );
  }
  const quoting = quotingMessage(messages, wanted);
  if (quoting !== undefined) {
    return drop("line-quoted", `message ${quoting} of the case's messages quotes it`);
  }
  if (answer.restatements.length > 0) {
    const indices = [...new Set(answer.restatements.map((each) => each.messageIndex))];
    return drop(
      "history-restates-line",
      `the case reader finds the line's instruction stated in message${indices.length === 1 ? "" : "s"} ${indices.join(", ")} of the session, before the step at message ${stepIndex}`
    );
  }

  // The profile: from the reads before the step. Its GitHub reads were made for the reads the
  // reader could reach, so they are made once.
  let profiled: ProfileResult;
  try {
    profiled = await caseProfile(run, { ...profileInput, reads: shownReads(messages) });
  } catch (error) {
    return drop("profile-failed", messageFor(error));
  }
  if (
    !profiled.profile.skills.some((each) => each.name === run.skill && each.repo === run.cut.repo)
  ) {
    return drop("not-cut-copy", notInstalled(run));
  }

  const digest = lastBefore(t.branch, stepItem.at, (entry) =>
    entry.type === "custom" &&
    entry.customType === "system-prompt-digest" &&
    typeof entry.data === "string"
      ? entry.data
      : undefined
  );
  const hash = pathHash(transcriptPath);
  const secrets = scrubbed({
    messages,
    systemPrompt: init?.systemPrompt,
    realStep: t.message(stepIndex),
    violatingStep: {
      role: "assistant",
      content: [
        ...(answer.violatingStep.text.trim() === ""
          ? []
          : [{ type: "text", text: answer.violatingStep.text }]),
        ...answer.violatingStep.toolCalls.map((call, n) => ({
          type: "toolCall",
          id: `violating_${n + 1}`,
          name: call.name,
          arguments: call.arguments,
        })),
      ],
    },
    criterion: answer.criterion,
  });
  const skillReadIndices = caseReads.map((each) => each.index);

  // The whole history before the step, as the case keeps it, for a restatement of the line: every
  // message but the skill's own read results, window by window, each window one `restate` call.
  const leftOut = new Set([
    ...skillReadIndices,
    ...skillReadResults(secrets.value.messages, run.skill),
  ]);
  const check = restatementWindows(secrets.value.messages, leftOut);
  const total = check.windows.length;
  record.restatementCheck = { read: 0, before: check.checked, windows: total };
  if (total > RESTATE_WINDOWS) {
    return drop(
      "restatement-check-incomplete",
      `the ${check.checked} messages before the step need ${total} windows of the restatement check, more than ${RESTATE_WINDOWS}`
    );
  }
  for (const [n, window] of check.windows.entries()) {
    let found: RestateAnswer;
    if (session.stopped) throw new NotNeeded();
    try {
      const result = await runReader(
        "restate",
        { line: answer.line.text, messages: window.messages },
        run.gateway
      );
      record.readerUsd += usdOf(result.spent);
      found = RestateAnswer.parse(result.answer);
    } catch (error) {
      if (error instanceof BudgetSpent) throw error;
      if (error instanceof ReaderFailed) record.readerUsd += usdOf(error.spent);
      return drop(
        "restatement-check-incomplete",
        `window ${n + 1} of ${total} of the restatement check was not read: ${messageFor(error)}`
      );
    }
    record.restatementCheck.read += window.completes.length;
    if (found.restatements.length > 0) {
      const indices = [...new Set(found.restatements.map((each) => each.messageIndex))];
      const quotes = found.restatements
        .map((each) => `${each.messageIndex}: ${JSON.stringify(cut(each.quote, 200))}`)
        .join("; ");
      return drop(
        "history-restates-line",
        `the restatement check finds the line's instruction stated in message${indices.length === 1 ? "" : "s"} ${indices.join(", ")} of the case, before the step (window ${n + 1} of ${total}): ${quotes}`
      );
    }
  }

  // The audit of the real step: the line and the messages before the step as the judge reads them,
  // and no criterion.
  let audit: AuditAnswer;
  if (session.stopped) throw new NotNeeded();
  try {
    const result = await runReader(
      "audit",
      {
        line: answer.line.text,
        before: beforeText({
          messages: secrets.value.messages,
          skill: run.skill,
          skillReads: skillReadIndices,
        }),
        step: stepText(secrets.value.realStep.content ?? []),
      },
      run.gateway
    );
    record.readerUsd += usdOf(result.spent);
    audit = AuditAnswer.parse(result.answer);
  } catch (error) {
    if (error instanceof BudgetSpent) throw error;
    if (error instanceof ReaderFailed) record.readerUsd += usdOf(error.spent);
    return drop("audit-failed", messageFor(error));
  }
  record.audit = audit;
  if (audit.verdict === "breaks") return drop("audit-breaks", audit.reason);
  if (audit.verdict === "not-governed") return drop("audit-not-governed", audit.reason);
  const built: Case = Case.parse({
    id: `${run.cut.id}-${hash.slice(0, 12)}`,
    cut: run.cut.id,
    skill: run.skill,
    baseFile: file.path,
    line: { text: answer.line.text, lineNumber, lineCount: baseLines.length },
    criterion: secrets.value.criterion,
    model,
    thinking: thinking.data,
    context,
    profile:
      init === undefined || secrets.value.systemPrompt === undefined
        ? profiled.profile
        : { ...profiled.profile, systemPrompt: secrets.value.systemPrompt, tools: init.tools },
    messages: secrets.value.messages,
    skillReads: skillReadIndices,
    realStep: secrets.value.realStep,
    violatingStep: secrets.value.violatingStep,
    source: { store: store.name, pathHash: hash },
    sessionStart,
    secretsRemoved: secrets.removed,
    sessionDigest: digest ?? null,
    evidence: [
      ...evidence,
      ...profiled.evidence,
      {
        item: `${AUDIT_EVIDENCE}${audit.verdict}`,
        source: `the audit reader, given the line, the messages before the step as the judge reads them and the real step, and no criterion: ${audit.reason}`,
      },
    ],
  });
  return { case: built, step: stepIndex };
}

/** One model's line of the summary's `byModel`. */
interface ModelCounts {
  tried: number;
  drops: Partial<Record<DropReason, number>>;
}

/** The sessions tried and the drops by reason of each model, a session under its record's
 *  `model`, or under `none` when the build found no model of it. */
function byModel(sessions: readonly SessionRecord[]): Record<string, ModelCounts> {
  const counts: Record<string, ModelCounts> = {};
  for (const session of sessions) {
    const key = session.model ?? "none";
    const row = counts[key] ?? { tried: 0, drops: {} };
    counts[key] = row;
    row.tried += 1;
    if (session.reason !== undefined) {
      row.drops[session.reason] = (row.drops[session.reason] ?? 0) + 1;
    }
  }
  return counts;
}

/** The copies output under `copiesDir` as the profile reads it (attribute.ts's `readR4`, the one
 *  reader of it), and attribute.ts's placing of reads for the count under `countDir` and that
 *  output, the two attribute.ts made the sessions files from. Throws a refusal when that output
 *  cannot be read as attribute.ts reads it, when the two do not place reads as attribute.ts does,
 *  or when the count holds no query of a configured store. */
function readCopiesAndPlacer(
  config: SkillCutsConfig,
  countDir: string,
  copiesDir: string
): { copies: Copies; placer: Placer } {
  let r4: R4Output;
  try {
    r4 = readR4(copiesDir);
  } catch (error) {
    throw new Refused(`--copies ${copiesDir}: ${messageFor(error)}`);
  }
  let placing: Placer;
  let outputs: number;
  try {
    const count = readCount(
      countDir,
      config.stores.map((store) => store.name)
    );
    outputs = count.outputs.length;
    placing = placer(count.outputs, r4);
  } catch (error) {
    throw new Refused(
      `--count ${countDir} and --copies ${copiesDir} do not place reads as attribute.ts does: ${messageFor(error)}`
    );
  }
  if (outputs === 0) {
    throw new Refused(`--count ${countDir} holds no query output of a store of the configuration`);
  }
  return {
    copies: {
      prefixes: r4.prefixes,
      installers: r4.installers,
      manifests: r4.manifests,
      branches: new Map(r4.repositories.map((row) => [row.repository, row.branch])),
    },
    placer: placing,
  };
}

/** The evidence a case's context gives (caseOf), which comes before its profile's. */
const CONTEXT_EVIDENCE = /^(subagent |role |no role$|the recorded system prompt and tools$)/;
/** The start of the item of a case's audit evidence, which comes after its profile's. */
const AUDIT_EVIDENCE = "audit of the real step: ";
/** The item of the profile's evidence for a Legion session whose working directory is no
 *  workspace, so its repository's AGENTS.md is left out. */
const NO_WORKSPACE = "left out: the session repository's AGENTS.md";

/**
 * `build-cases --reprofile`: the cases of a case file, each with its profile built again from the
 * case itself, with no session read and no reader call. A case holds what the profile is built
 * from but the session's working directory: its store, its messages (their `[Skill file: …]`
 * lines), its start and whether it carries a recorded prompt. The working directory is taken as
 * the workspace of the repository whose AGENTS.md the old profile holds; a case whose old profile
 * holds none keeps the old evidence of why. Only `profile` (its skills, plugins and context files)
 * and the profile's part of `evidence` change. A case whose profile cannot be built, or no longer
 * installs its skill, is left out and named, and the command fails.
 */
async function reprofileCases(
  values: Readonly<Record<string, string | undefined>>,
  io: BuildIo
): Promise<number> {
  const given = required(
    values,
    ["config", "old", "copies", "count", "reprofile", "out"],
    BUILD_CASES_USAGE
  );
  for (const flag of [
    "key-file",
    "cut",
    "skill",
    "sessions",
    "seed",
    "max",
    "budget-usd",
    "concurrency",
    "stop-projected-usd",
    "stop-after-sessions",
    "stop-zero-case-usd",
  ]) {
    if (values[flag] !== undefined) {
      throw new Refused(
        `--reprofile reads no session and calls no reader, so it takes no --${flag}`
      );
    }
  }
  if (path.resolve(given.reprofile) === path.resolve(given.out)) {
    throw new Refused(`--out ${given.out} is the file --reprofile reads; give a new file`);
  }
  refuseOutInCheckout(given.out);
  const config = configFrom(given.config);
  mustBeFile(path.join(given.old, "SKILL.md"), "--old's SKILL.md");
  mustBeFile(given.reprofile, "--reprofile");
  let cases: Case[];
  try {
    cases = loadCases(given.reprofile, given.old);
  } catch (error) {
    throw new Refused(`--reprofile ${given.reprofile}: ${messageFor(error)}`);
  }
  const { copies, placer: placing } = readCopiesAndPlacer(config, given.count, given.copies);
  const run: ProfileRun = {
    config,
    copies,
    github: new GithubReads(io.gh, copies.branches),
    placer: placing,
  };
  const rebuilt: Case[] = [];
  const records: Record<string, unknown>[] = [];
  const leftOut: { id: string; why: string }[] = [];
  for (const c of cases) {
    const leave = (why: string) => {
      leftOut.push({ id: c.id, why });
      io.out(`build-cases: ${c.id}: left out: ${why}\n`);
    };
    const store = config.stores.find((each) => each.name === c.source.store);
    const cut = config.cuts.find((each) => each.id === c.cut);
    if (store === undefined || cut === undefined) {
      leave(
        store === undefined
          ? `its store ${c.source.store} is no store of the configuration`
          : `its cut ${c.cut} is no cut of the configuration`
      );
      continue;
    }
    const agents = c.profile.contextFiles.find((file) => file.path === "AGENTS.md");
    let profiled: ProfileResult;
    try {
      profiled = await caseProfile(run, {
        store: store.name,
        context: store.context,
        project: store.context.startsWith("legion:")
          ? store.context.slice("legion:".length)
          : undefined,
        reads: shownReads(c.messages),
        sessionStart: c.sessionStart,
        cwd: agents === undefined ? undefined : `/legion/workspaces/${agents.repo}/`,
        recordedPrompt: c.profile.systemPrompt !== undefined,
      });
    } catch (error) {
      leave(`${DROP_REASONS["profile-failed"]}: ${messageFor(error)}`);
      continue;
    }
    if (!profiled.profile.skills.some((each) => each.name === c.skill && each.repo === cut.repo)) {
      leave(notInstalled({ skill: c.skill, cut }));
      continue;
    }
    const old = c.evidence ?? [];
    const profileStart = old.findIndex((each) => !CONTEXT_EVIDENCE.test(each.item));
    // Without the old profile's AGENTS.md the working directory is not known, so the old evidence
    // of why it is left out stands in for the new one's.
    const why = agents === undefined ? old.filter((each) => /AGENTS\.md$/.test(each.item)) : [];
    const evidence = [
      ...old.slice(0, profileStart < 0 ? old.length : profileStart),
      ...profiled.evidence.flatMap((each) =>
        each.item === NO_WORKSPACE && why.length > 0 ? why : [each]
      ),
      ...old.filter((each) => each.item.startsWith(AUDIT_EVIDENCE)),
    ];
    const built = Case.parse({ ...c, profile: { ...c.profile, ...profiled.profile }, evidence });
    rebuilt.push(built);
    const names = (profile: Profile) => profile.skills.map((skill) => skill.name);
    const before = names(c.profile);
    const after = names(built.profile);
    records.push({
      id: c.id,
      skills: after.length,
      added: after.filter((name) => !before.includes(name)),
      removed: before.filter((name) => !after.includes(name)),
      plugins: built.profile.plugins.map((plugin) => plugin.name),
      contextFiles: built.profile.contextFiles.map((file) => `${file.repo} ${file.path}`),
    });
    io.out(
      `build-cases: ${c.id}: profile of ${after.length} skills (${before.length} before), each in its repository at its commit\n`
    );
  }
  writeWhole(given.out, rebuilt.map((each) => `${JSON.stringify(each)}\n`).join(""));
  const summaryFile = `${given.out}.summary.json`;
  writeWhole(
    summaryFile,
    json({
      command: "build-cases --reprofile",
      from: given.reprofile,
      casesRead: cases.length,
      casesWritten: rebuilt.length,
      cases: records,
      leftOut,
    })
  );
  // The file must load as the behaviour commands load it.
  if (rebuilt.length > 0) loadCases(given.out, given.old);
  io.out(
    `build-cases: ${rebuilt.length} of ${cases.length} cases written with their profiles built again, to ${given.out} and ${summaryFile}\n`
  );
  if (leftOut.length > 0) {
    io.err(
      `build-cases: failed: ${leftOut.length} cases left out: ${leftOut.map((each) => each.id).join(", ")}`
    );
    return EXIT.failed;
  }
  return EXIT.ok;
}

/** What a build-cases run is made from, as its summary records it: a resumed run must be made
 *  from the same. The texts are digests of what the run read (`digestOf`); the paths are only
 *  for a reader of the summary. */
const CasesInputs = z.strictObject({
  cut: z.string(),
  skill: z.string(),
  seed: z.string(),
  old: Digested,
  sessions: Digested,
  copies: Digested,
  count: Digested,
});
type CasesInputs = z.infer<typeof CasesInputs>;

/** The files of the copies output that build-cases reads (readCopies, and attribute.ts's
 *  readR4). */
const COPIES_READ = [
  "installers.tsv",
  "loaders.tsv",
  "manifests.tsv",
  "prefix-table.tsv",
  "repositories.tsv",
  "skill-copies.tsv",
];

/** The query outputs of `countDir` that attribute.ts's readCount reads for `stores`, and its
 *  run log, as paths inside it, in order. */
function countFiles(countDir: string, stores: readonly string[]): string[] {
  const files: string[] = [];
  for (const store of [...stores].sort()) {
    const dir = path.join(countDir, store);
    if (!existsSync(dir)) continue;
    for (const file of readdirSync(dir).sort()) {
      if (/^\d{4}-\d{2}-\d{2}(T\d{2})?\.txt$/.test(file)) files.push(`${store}/${file}`);
    }
  }
  return [...files, "run.log"];
}

/** Where two runs' inputs differ, one phrase each; empty when they are the same. */
function inputsDiffer(was: CasesInputs, now: CasesInputs): string[] {
  const differ: string[] = [];
  for (const key of ["cut", "skill", "seed"] as const) {
    if (was[key] !== now[key]) differ.push(`--${key} was ${was[key]}, now ${now[key]}`);
  }
  for (const key of ["old", "sessions", "copies", "count"] as const) {
    if (was[key].sha256 !== now[key].sha256) {
      differ.push(
        `--${key} was ${was[key].path} (sha256 ${was[key].sha256.slice(0, 12)}), now ${now[key].path} (sha256 ${now[key].sha256.slice(0, 12)})`
      );
    }
  }
  return differ;
}

/** A build-cases summary as a resumed run reads it back. */
const CasesSummary = z.looseObject({
  command: z.literal("build-cases"),
  inputs: CasesInputs.optional(),
  sessions: z.array(
    z.looseObject({
      store: z.string(),
      hash: z.string(),
      outcome: z.string(),
      reason: z.string().optional(),
    })
  ),
  cases: z.array(z.looseObject({ id: z.string() })),
  sessionQueries: z.number(),
  readerSpend: SpendRecord,
});

/** One session an earlier run decided: its summary record and, for a case, the case's summary
 *  record (the case itself stays in the cases file). */
interface DecidedSession {
  readonly record: SessionRecord;
  readonly caseRecord?: Record<string, unknown>;
}

/** What an earlier run of the same inputs decided, by `<store>\0<hash>`, and what it spent and
 *  queried. */
interface PriorBuild {
  readonly decided: ReadonlyMap<string, DecidedSession>;
  readonly spend: Spend;
  readonly sessionQueries: number;
}

/** Writes the cases file `out` again with only its lines of a case whose id `ids` holds (each
 *  once, in the file's order), leaving out a line cut short; returns the lines kept, by id. */
function keepCases(out: string, ids: ReadonlySet<string>): Map<string, string> {
  const lines = new Map<string, string>();
  if (existsSync(out)) {
    for (const line of readFileSync(out, "utf8").split("\n")) {
      let id: unknown;
      try {
        id = (JSON.parse(line) as { id?: unknown }).id;
      } catch {
        continue;
      }
      if (typeof id === "string" && ids.has(id) && !lines.has(id)) lines.set(id, line);
    }
  }
  writeWhole(out, [...lines.values()].map((line) => `${line}\n`).join(""));
  return lines;
}

/**
 * The earlier run whose files are at `out` and `summaryFile`, or undefined when neither is there.
 * Refuses a cases file with no summary, a summary that is not one of build-cases or records no
 * inputs, and a summary of other inputs, naming each difference. A run appends each case to the
 * cases file and then writes its summary, so a kill can leave a case line (whole, or cut short)
 * that the summary does not record: the cases file is written again with only the cases the
 * summary records, and their sessions are decided. A session dropped is decided; one the summary
 * records as a case that the file lacks, one the budget stopped and one not needed are not.
 */
function priorBuild(out: string, summaryFile: string, inputs: CasesInputs): PriorBuild | undefined {
  if (!existsSync(summaryFile)) {
    if (existsSync(out)) {
      throw new Refused(
        `--out ${out} is there but ${summaryFile} is not, so what built it is not known; give another --out`
      );
    }
    return undefined;
  }
  const parsed = CasesSummary.safeParse(readJson(summaryFile));
  if (!parsed.success) {
    throw new Refused(
      `${summaryFile} is no build-cases summary this build can go on from: ${schemaFaults(parsed.error, "the file")}`
    );
  }
  const summary = parsed.data;
  if (summary.inputs === undefined) {
    throw new Refused(
      `${summaryFile} records no inputs (an older build-cases wrote it), so a build cannot go on from it; give another --out`
    );
  }
  const differ = inputsDiffer(summary.inputs, inputs);
  if (differ.length > 0) {
    throw new Refused(
      `--out ${out} holds a build of other inputs, so this one does not go on from it: ${differ.join("; ")}`
    );
  }
  const recorded = new Map(summary.cases.map((each) => [each.id, each]));
  const lines = keepCases(out, new Set(recorded.keys()));
  const decided = new Map<string, DecidedSession>();
  for (const session of summary.sessions) {
    const record = session as unknown as SessionRecord;
    const key = `${session.store}\0${session.hash}`;
    if (session.reason !== undefined) {
      decided.set(key, { record });
      continue;
    }
    const id = /^case (\S+)$/.exec(session.outcome)?.[1];
    const caseRecord = id === undefined || !lines.has(id) ? undefined : recorded.get(id);
    if (caseRecord !== undefined) decided.set(key, { record, caseRecord });
  }
  return { decided, spend: summary.readerSpend, sessionQueries: summary.sessionQueries };
}

/** `skill-cuts build-cases`. */
async function buildCases(argv: readonly string[], io: BuildIo): Promise<number> {
  const values = parse(
    argv,
    {
      config: { type: "string" },
      "key-file": { type: "string" },
      cut: { type: "string" },
      skill: { type: "string" },
      old: { type: "string" },
      sessions: { type: "string" },
      seed: { type: "string" },
      copies: { type: "string" },
      count: { type: "string" },
      reprofile: { type: "string" },
      out: { type: "string" },
      max: { type: "string" },
      "budget-usd": { type: "string" },
      concurrency: { type: "string" },
      "stop-projected-usd": { type: "string" },
      "stop-after-sessions": { type: "string" },
      "stop-zero-case-usd": { type: "string" },
    },
    BUILD_CASES_USAGE
  );
  if (values.reprofile !== undefined) return reprofileCases(values, io);
  const given = required(
    values,
    ["config", "key-file", "cut", "skill", "old", "sessions", "seed", "copies", "count", "out"],
    BUILD_CASES_USAGE
  );
  // The cases file and its spool hold real session text.
  refuseOutInCheckout(given.out);
  if (!/^\d+$/.test(given.seed)) throw new Refused(`--seed ${given.seed} is not a whole number`);
  const max = values.max === undefined ? Number.POSITIVE_INFINITY : Number(values.max);
  if (values.max !== undefined && (!/^\d+$/.test(values.max) || max < 1)) {
    throw new Refused(`--max ${values.max} is not a whole number of 1 or more`);
  }
  /** The dollars `text` gives for `--<flag>`, or undefined when it is not given. Throws a refusal
   *  on a text that is no number of dollars of 0 or more. */
  const dollars = (flag: string, text: string | undefined): number | undefined => {
    if (text === undefined) return undefined;
    const usd = Number(text);
    if (text.trim() === "" || !(usd >= 0)) {
      throw new Refused(`--${flag} ${text} is not a number of dollars of 0 or more`);
    }
    return usd;
  };
  const budget = values["budget-usd"];
  const budgetUsd = dollars("budget-usd", budget);
  const concurrency =
    values.concurrency === undefined ? BUILD_CONCURRENCY : Number(values.concurrency);
  if (values.concurrency !== undefined && (!/^\d+$/.test(values.concurrency) || concurrency < 1)) {
    throw new Refused(`--concurrency ${values.concurrency} is not a whole number of 1 or more`);
  }
  const stopRule: StopRule = {
    projectedUsd: dollars("stop-projected-usd", values["stop-projected-usd"]) ?? STOP_PROJECTED_USD,
    afterSessions:
      values["stop-after-sessions"] === undefined
        ? STOP_AFTER_SESSIONS
        : Number(values["stop-after-sessions"]),
    zeroCaseUsd: dollars("stop-zero-case-usd", values["stop-zero-case-usd"]) ?? STOP_ZERO_CASE_USD,
  };
  if (values["stop-after-sessions"] !== undefined && !/^\d+$/.test(values["stop-after-sessions"])) {
    throw new Refused(
      `--stop-after-sessions ${values["stop-after-sessions"]} is not a whole number`
    );
  }
  const config = configFrom(given.config);
  const cut = config.cuts.find((each) => each.id === given.cut);
  if (cut === undefined) {
    throw new Refused(
      `--cut ${given.cut} is no cut of the configuration (${config.cuts.map((each) => each.id).join(", ")})`
    );
  }
  mustBeFile(path.join(given.old, "SKILL.md"), "--old's SKILL.md");
  mustBeFile(given["key-file"], "--key-file");
  const files = skillFiles(given.old);
  const { copies, placer: placing } = readCopiesAndPlacer(config, given.count, given.copies);
  const candidates = readCandidates(given.sessions, config);
  const order = blindOrder(candidates, given.seed).map(({ item }) => item);

  // An earlier run of the same inputs at --out: every session it decided is kept as it was, with
  // no pull and no reader call, and the build goes on from the first session it did not decide.
  const summaryFile = `${given.out}.summary.json`;
  const inputs: CasesInputs = {
    cut: cut.id,
    skill: given.skill,
    seed: given.seed,
    old: digestOf(
      given.old,
      files.map((file) => file.path)
    ),
    sessions: {
      ...digestOf(path.dirname(given.sessions), [path.basename(given.sessions)]),
      path: given.sessions,
    },
    copies: digestOf(given.copies, COPIES_READ),
    count: digestOf(
      given.count,
      countFiles(
        given.count,
        config.stores.map((store) => store.name)
      )
    ),
  };
  const prior = priorBuild(given.out, summaryFile, inputs);
  const decided: ReadonlyMap<string, DecidedSession> = prior?.decided ?? new Map();
  const priorSpend = prior?.spend ?? NO_SPEND;
  if (prior === undefined) writeWhole(given.out, "");
  else {
    const kept = [...decided.values()].filter((each) => each.caseRecord !== undefined).length;
    io.out(
      `build-cases: going on from ${summaryFile}: ${decided.size} sessions decided there (${kept} cases), $${priorSpend.usd.toFixed(4)} of readers spent\n`
    );
  }
  const keyOf = (candidate: { store: string; hash: string }) =>
    `${candidate.store}\0${candidate.hash}`;

  // The store is read one query at a time, whichever sessions are open.
  let storeTurn: Promise<unknown> = Promise.resolve();
  const oneAtATime = <T>(read: () => Promise<T>): Promise<T> => {
    const next = storeTurn.then(read, read);
    storeTurn = next.catch(() => undefined);
    return next;
  };

  // A run that goes on checks the stop rule on what the earlier runs decided before it reads the
  // store at all, so a build the rule stopped stops again at once.
  const stopAtOnce =
    prior === undefined
      ? undefined
      : stopBy(
          stopRule,
          {
            sessions: decided.size,
            cases: [...decided.values()].filter((each) => each.caseRecord !== undefined).length,
            spentUsd: priorSpend.usd,
          },
          max
        );

  // Paths for the candidates that lack one and are not decided yet, per store.
  const paths = new Map<string, string>();
  let sessionQueries = prior?.sessionQueries ?? 0;
  for (const store of stopAtOnce === undefined ? config.stores : []) {
    const hashes = order
      .filter(
        (each) => each.store === store.name && each.path === undefined && !decided.has(keyOf(each))
      )
      .map((each) => each.hash);
    if (hashes.length === 0) continue;
    const resolved = await resolvePaths(config.psqlCommand, store.name, hashes);
    sessionQueries += resolved.queries;
    for (const [hash, filePath] of resolved.paths) paths.set(`${store.name}\0${hash}`, filePath);
  }

  // The budget counts what the earlier runs spent; the gateway takes as many calls at once as
  // sessions are open.
  const run: CaseRun = {
    config,
    cut,
    skill: given.skill,
    old: given.old,
    files,
    copies,
    github: new GithubReads(io.gh, copies.branches),
    gateway: readerGateway(
      config,
      given["key-file"],
      budgetUsd === undefined ? undefined : Math.max(0, budgetUsd - priorSpend.usd),
      concurrency
    ),
    placer: placing,
    servedIds: new Set(config.servedModels.map(modelId)),
  };
  const caseRecords: Record<string, unknown>[] = [];
  const sessions: SessionRecord[] = [];
  let casesBuilt = 0;
  // The sessions decided so far, the earlier runs' among them, for the stop rule.
  let decidedSessions = 0;
  let stopped: string | undefined;
  let ruleStop: RuleStop | undefined;
  const spools = spoolDir(given.out);
  const write = () => {
    const drops: Partial<Record<DropReason, number>> = {};
    let pulls = 0;
    for (const session of sessions) {
      if (session.reason !== undefined) drops[session.reason] = (drops[session.reason] ?? 0) + 1;
      pulls += session.pulls ?? 0;
    }
    writeWhole(
      summaryFile,
      json({
        command: "build-cases",
        cut: cut.id,
        skill: given.skill,
        seed: given.seed,
        inputs,
        order: order.map(({ store, hash }) => ({ store, hash })),
        bounds: {
          readerWindows: READER_WINDOWS,
          readerMessages: READER_MESSAGES,
          readerChars: READER_CHARS,
          readerEarlierMessages: READER_EARLIER_MESSAGES,
          readerEarlierChars: READER_EARLIER_CHARS,
          restateChars: RESTATE_CHARS,
          restateWindows: RESTATE_WINDOWS,
          restatePartChars: RESTATE_PART_CHARS,
          restateOverlap: RESTATE_OVERLAP,
          messageChars: MESSAGE_CHARS,
          pullPage: PULL_PAGE,
          max: Number.isFinite(max) ? max : null,
          budgetUsd: budgetUsd ?? null,
          concurrency,
          stopProjectedUsd: stopRule.projectedUsd,
          stopAfterSessions: stopRule.afterSessions,
          stopZeroCaseUsd: stopRule.zeroCaseUsd,
        },
        stopped: ruleStop ?? stopped ?? null,
        sessionsTried: sessions.length,
        casesBuilt,
        drops,
        byModel: byModel(sessions),
        sessions,
        cases: caseRecords,
        sessionQueries,
        pullCalls: pulls,
        readerSpend: addSpend(priorSpend, run.gateway.spend()),
      })
    );
  };
  // A run that goes on leaves the earlier summary as it is until it decides a session: by then it
  // has gone past every session the earlier run decided (they come first in the seeded order), so
  // a kill at any moment leaves a summary that records them all.
  if (prior === undefined) write();

  // One session's work: its pull, then caseOf, until it is no longer wanted. The promise never
  // rejects: it gives the outcome, or what stopped it.
  type Worked =
    | { readonly outcome: Outcome }
    | { readonly budget: BudgetSpent }
    | { readonly notNeeded: true }
    | { readonly error: unknown };
  interface Open {
    readonly record: SessionRecord;
    readonly wanted: { stopped: boolean };
    readonly worked: Promise<Worked>;
  }
  const work = (candidate: (typeof order)[number]): Open => {
    const store = storeNamed(config, candidate.store);
    const record: SessionRecord = {
      store: store.name,
      hash: candidate.hash,
      readerUsd: 0,
      outcome: "",
    };
    const wanted = { stopped: false };
    const transcriptPath = candidate.path ?? paths.get(`${store.name}\0${candidate.hash}`);
    const spool = path.join(spools, `${store.name}-${candidate.hash}.jsonl`);
    const outcome = async (): Promise<Outcome> => {
      if (transcriptPath === undefined) {
        return { drop: "not-in-store", detail: "sessions.sql gave no row of this hash" };
      }
      record.pathHash = pathHash(transcriptPath);
      let pulled: { bytes: number; calls: number };
      try {
        pulled = await oneAtATime(() => {
          if (wanted.stopped) throw new NotNeeded();
          return pullTranscript(config.psqlCommand, store.name, transcriptPath, spool);
        });
      } catch (error) {
        if (error instanceof NotNeeded) throw error;
        return { drop: "pull-failed", detail: messageFor(error) };
      }
      record.pulls = pulled.calls;
      if (pulled.bytes === 0) return { drop: "pull-failed", detail: "the pull returned no row" };
      return caseOf(run, store, transcriptPath, spool, record, wanted);
    };
    const worked = outcome()
      .finally(() => rmSync(spool, { force: true }))
      .then(
        (each): Worked => ({ outcome: each }),
        (error: unknown): Worked =>
          error instanceof NotNeeded
            ? { notNeeded: true }
            : error instanceof BudgetSpent
              ? { budget: error }
              : { error }
      );
    return { record, wanted, worked };
  };

  // At most `concurrency` sessions are open (being read, or read and waiting for those before
  // them), each pulled into its spool file and read from there. Their outcomes are decided in the
  // seeded order, as one at a time would decide them, so the cases kept are the first --max
  // cases of that order. Once the build ends, each session still open is stopped before its next
  // reader call, and recorded with what it spent.
  const open = new Map<number, Open>();
  let launched = 0;
  const fill = () => {
    while (open.size < concurrency && launched < order.length) {
      const position = launched++;
      const candidate = order[position];
      if (candidate === undefined || decided.has(keyOf(candidate))) continue;
      open.set(position, work(candidate));
    }
  };
  const closeOpen = async (outcome: string) => {
    for (const each of open.values()) each.wanted.stopped = true;
    const left = [...open.entries()].sort(([a], [b]) => a - b);
    open.clear();
    await Promise.all(left.map(([, each]) => each.worked));
    for (const [, each] of left) {
      each.record.outcome = outcome;
      sessions.push(each.record);
    }
  };
  for (const [position, candidate] of order.entries()) {
    if (casesBuilt >= max) break;
    const kept = decided.get(keyOf(candidate));
    if (kept !== undefined) {
      sessions.push(kept.record);
      decidedSessions += 1;
      if (kept.caseRecord !== undefined) {
        caseRecords.push(kept.caseRecord);
        casesBuilt += 1;
      }
      continue;
    }
    // The stop rule, before the build reads on: after each session decided, and, in a run that
    // goes on, on those the earlier runs decided (`stopAtOnce`).
    ruleStop =
      stopAtOnce ??
      stopBy(
        stopRule,
        {
          sessions: decidedSessions,
          cases: casesBuilt,
          spentUsd: addSpend(priorSpend, run.gateway.spend()).usd,
        },
        max
      );
    if (ruleStop !== undefined) break;
    fill();
    const session = open.get(position);
    if (session === undefined) throw new Error(`session ${position + 1} was not opened`);
    const worked = await session.worked;
    open.delete(position);
    const { record } = session;
    const store = record.store;
    if ("error" in worked || "notNeeded" in worked) {
      await closeOpen("not-needed");
      rmSync(spools, { recursive: true, force: true });
      // The session whose turn it is was never told to stop.
      throw "error" in worked ? worked.error : new Error(`session ${position + 1} was stopped`);
    }
    if ("budget" in worked) {
      const spent = addSpend(priorSpend, run.gateway.spend()).usd;
      stopped = `the readers' spend reached --budget-usd ${budget}: $${spent.toFixed(4)} at list price, so no new reader call starts`;
      record.outcome = "stopped before its case was built: the budget is spent";
      sessions.push(record);
      await closeOpen(record.outcome);
      write();
      break;
    }
    const { outcome } = worked;
    sessions.push(record);
    decidedSessions += 1;
    if ("drop" in outcome) {
      record.outcome = "dropped";
      record.reason = outcome.drop;
      record.detail = outcome.detail;
      io.out(
        `build-cases: ${position + 1}/${order.length} ${store} ${candidate.hash}: dropped (${outcome.drop}) ${outcome.detail}\n`
      );
    } else {
      const built = outcome.case;
      record.outcome = `case ${built.id}`;
      // A case line is appended whole before the summary records it; a run that goes on from a
      // kill between the two leaves out a line its summary does not record.
      appendFileSync(given.out, `${JSON.stringify(built)}\n`);
      casesBuilt += 1;
      caseRecords.push({
        id: built.id,
        pathHash: built.source.pathHash,
        store: built.source.store,
        model: built.model,
        thinking: built.thinking,
        context: built.context,
        step: outcome.step,
        recordedPrompt: built.profile.systemPrompt !== undefined,
        windows: record.windows,
        messages: built.messages.length,
        skillReads: built.skillReads,
        baseFile: built.baseFile,
        line: built.line,
        secretsRemoved: built.secretsRemoved,
        profile: {
          skills: built.profile.skills.length,
          plugins: built.profile.plugins.map((plugin) => plugin.name),
          contextFiles: built.profile.contextFiles.map((file) => `${file.repo} ${file.path}`),
        },
      });
      io.out(
        `build-cases: ${position + 1}/${order.length} ${store} ${candidate.hash}: case ${built.id} (step ${outcome.step}, ${built.model}, ${built.context})\n`
      );
    }
    write();
  }
  const left = open.size > 0;
  if (left) await closeOpen("not-needed");
  rmSync(spools, { recursive: true, force: true });
  if (left || prior !== undefined || ruleStop !== undefined) write();
  // A run that goes on with a smaller --max than the earlier one records fewer cases; the cases
  // file keeps only those (after the summary, so a kill between leaves lines the next run drops).
  if (prior !== undefined) {
    keepCases(given.out, new Set(caseRecords.map((each) => String(each.id))));
  }
  // The file must load as the behaviour commands load it.
  loadCases(given.out, given.old);
  const usd = addSpend(priorSpend, run.gateway.spend()).usd;
  io.out(
    `build-cases: ${casesBuilt} cases of ${sessions.length} sessions tried, $${usd.toFixed(4)} of readers at list price; written to ${given.out} and ${summaryFile}\n`
  );
  if (stopped !== undefined) {
    io.err(`build-cases: stopped: ${stopped}`);
    return EXIT.stopped;
  }
  if (ruleStop !== undefined) {
    const projected =
      ruleStop.projectedUsd === null ? "unbounded" : `$${ruleStop.projectedUsd.toFixed(4)}`;
    io.err(
      `build-cases: stopped by --${ruleStop.rule}: ${ruleStop.sessions} sessions decided, ${ruleStop.cases} cases, $${ruleStop.spentUsd.toFixed(4)} of readers spent, projected ${projected} to --max ${Number.isFinite(max) ? max : "(none)"} cases`
    );
    return EXIT.stoppedByCost;
  }
  return EXIT.ok;
}

// ---------------------------------------------------------------------------------------------
// build-tasks

const TaskRow = z.strictObject({ path: z.string().min(1), text: z.string() });

/** One first user message a query of build-tasks found, as `<out>.found.jsonl` keeps it: the
 *  query (store and day), the file's path hash and the text. */
const FoundRow = z.strictObject({
  store: z.string(),
  day: z.string(),
  pathHash: z.string().regex(/^[0-9a-f]{64}$/),
  /** The file hash sessions.sql finds the file by (`fileHash`), so `--profiles-out` can pull the
   *  session; a row an older build-tasks found has none. */
  hash: z
    .string()
    .regex(/^[0-9a-f]{16}$/)
    .optional(),
  text: z.string(),
});
type FoundRow = z.infer<typeof FoundRow>;

/** One query of build-tasks: its store and day, and its row count, or its error. */
const TasksQuery = z.strictObject({
  store: z.string(),
  day: z.string(),
  rows: z.number().nullable(),
  error: z.string().optional(),
});
type TasksQuery = z.infer<typeof TasksQuery>;

/** What a build-tasks run is made from: the skill's SKILL.md, the stores, the window, and the
 *  model `--model` names, when it names one. */
const TasksInputs = z.strictObject({
  skill: Digested,
  stores: z.array(z.string()),
  window: z.strictObject({ start: z.string(), end: z.string() }),
  model: z.string().optional(),
});
type TasksInputs = z.infer<typeof TasksInputs>;

/** A build-tasks summary as a resumed run reads it back: a finished one, or one in progress. */
const TasksSummary = z.looseObject({
  command: z.literal("build-tasks"),
  inputs: TasksInputs.optional(),
  done: z.boolean().optional(),
  terms: z.array(z.string()).nullable().optional(),
  queries: z.array(TasksQuery),
  readerSpend: SpendRecord,
});

/** The earlier build-tasks run at `out` and `summaryFile`, or undefined when neither is there;
 *  refuses as build-cases does (`priorBuild`). */
function priorTasks(
  out: string,
  summaryFile: string,
  inputs: TasksInputs
): { done: boolean; terms: string[] | undefined; queries: TasksQuery[]; spend: Spend } | undefined {
  if (!existsSync(summaryFile)) {
    if (existsSync(out)) {
      throw new Refused(
        `--out ${out} is there but ${summaryFile} is not, so what built it is not known; give another --out`
      );
    }
    return undefined;
  }
  const parsed = TasksSummary.safeParse(readJson(summaryFile));
  if (!parsed.success || parsed.data.inputs === undefined) {
    throw new Refused(
      `${summaryFile} is no build-tasks summary with its inputs, so a build cannot go on from it; give another --out`
    );
  }
  const was = parsed.data.inputs;
  const differ: string[] = [];
  if (was.skill.sha256 !== inputs.skill.sha256) {
    differ.push(
      `--skill was ${was.skill.path}, now ${inputs.skill.path}, and its SKILL.md differs`
    );
  }
  if (was.stores.join() !== inputs.stores.join()) {
    differ.push(`--store was ${was.stores.join(", ")}, now ${inputs.stores.join(", ")}`);
  }
  if (was.window.start !== inputs.window.start || was.window.end !== inputs.window.end) {
    differ.push(
      `the window was ${was.window.start} to ${was.window.end}, now ${inputs.window.start} to ${inputs.window.end}`
    );
  }
  if (was.model !== inputs.model) {
    differ.push(`--model was ${was.model ?? "not given"}, now ${inputs.model ?? "not given"}`);
  }
  if (differ.length > 0) {
    throw new Refused(
      `--out ${out} holds a build of other inputs, so this one does not go on from it: ${differ.join("; ")}`
    );
  }
  return {
    done: parsed.data.done === true,
    terms: parsed.data.terms ?? undefined,
    queries: parsed.data.queries,
    spend: parsed.data.readerSpend,
  };
}

/** The UTC days of [start, end), as YYYY-MM-DD. */
function windowDays(start: string, end: string): string[] {
  const days: string[] = [];
  const first = new Date(start);
  let at = Date.UTC(first.getUTCFullYear(), first.getUTCMonth(), first.getUTCDate());
  const stop = Date.parse(end);
  for (; at < stop; at += DAY_MS) days.push(new Date(at).toISOString().slice(0, 10));
  return days;
}

/** `items`, or at most `max` of them spread evenly, in their order. */
function spread<T>(items: readonly T[], max: number): T[] {
  if (items.length <= max) return [...items];
  return Array.from({ length: max }, (_, n) => items[Math.floor((n * items.length) / max)] as T);
}

/** What `--profiles-out` asks of a build-tasks run: where each task's profile goes, what the
 *  profiles are rebuilt from (build-cases' own ProfileRun: the configuration, `--copies`,
 *  `--count` and GitHub), and, for written tasks, the session whose profile they take. */
interface ProfilesPlan {
  readonly dir: string;
  readonly run: ProfileRun;
  readonly contextSession: { readonly store: string; readonly hash: string } | undefined;
}

/** The `--profiles-out` plan of `values`, checked before any store read or model call, or
 *  undefined when no profile is asked for. `written` says whether the run can write tasks, which
 *  take `--context-session`'s profile. */
function profilesPlan(
  values: Readonly<Record<string, unknown>>,
  config: SkillCutsConfig,
  io: BuildIo,
  written: boolean
): ProfilesPlan | undefined {
  const dir = values["profiles-out"];
  if (typeof dir !== "string") {
    for (const flag of ["copies", "count", "context-session"]) {
      if (values[flag] !== undefined) {
        throw new Refused(`--${flag} goes with --profiles-out only. ${BUILD_TASKS_USAGE}`);
      }
    }
    return undefined;
  }
  const copiesDir = values.copies;
  const countDir = values.count;
  if (typeof copiesDir !== "string" || typeof countDir !== "string") {
    throw new Refused(
      `--profiles-out needs --copies and --count, the copies output and the count build-cases reads. ${BUILD_TASKS_USAGE}`
    );
  }
  const named = values["context-session"];
  let contextSession: ProfilesPlan["contextSession"];
  if (written) {
    if (typeof named !== "string") {
      throw new Refused(
        `--profiles-out needs --context-session <store>:<hash>, the session whose profile a written task takes. ${BUILD_TASKS_USAGE}`
      );
    }
    const match = /^([^:\s]+):([0-9a-f]{16})$/.exec(named);
    if (match === null) {
      throw new Refused(`--context-session ${named} is not <store>:<16 hex characters>`);
    }
    const [, store = "", hash = ""] = match;
    try {
      storeNamed(config, store);
    } catch (error) {
      throw new Refused(messageFor(error));
    }
    contextSession = { store, hash };
  } else if (named !== undefined) {
    throw new Refused("--first-read writes no written task, so it takes no --context-session");
  }
  const { copies, placer: placing } = readCopiesAndPlacer(config, countDir, copiesDir);
  return {
    dir,
    contextSession,
    run: {
      config,
      copies,
      github: new GithubReads(io.gh, copies.branches),
      placer: placing,
    },
  };
}

/** The transcript of the file `filePath` of `store`, pulled into `spool` and parsed
 *  (`readTranscript`). Throws when the pull fails or returns no row, or a line is no entry. */
async function pulledTranscript(
  command: string,
  store: string,
  filePath: string,
  spool: string
): Promise<Transcript> {
  const { bytes } = await pullTranscript(command, store, filePath, spool);
  if (bytes === 0) throw new Error("the pull returned no row");
  return readTranscript(spool);
}

/** The profile of the file `filePath` of `store`, pulled into `spool` (`pulledTranscript`) and
 *  rebuilt by `sessionProfile`. Throws, naming why, when the session gives none. */
async function pulledProfile(
  run: ProfileRun,
  store: string,
  filePath: string,
  spool: string,
  skill: string
): Promise<SessionProfile> {
  const t = await pulledTranscript(run.config.psqlCommand, store, filePath, spool);
  return sessionProfile(run, storeNamed(run.config, store), filePath, t, skill);
}

/** The context session of a run and its profile, the one each written task takes. */
interface ContextProfile {
  readonly store: string;
  readonly hash: string;
  readonly pathHash: string;
  readonly built: SessionProfile;
}

/** The context session's profile, rebuilt before any reader call, or undefined when the plan
 *  names no context session. Throws when the session gives none, and refuses (exit 2) a session
 *  whose profile does not list the skill, since a written task in it would test nothing. */
async function contextProfile(
  plan: ProfilesPlan,
  spool: string,
  skill: string
): Promise<ContextProfile | undefined> {
  if (plan.contextSession === undefined) return undefined;
  const { store, hash } = plan.contextSession;
  const named = `--context-session ${store}:${hash}`;
  const { paths } = await resolvePaths(plan.run.config.psqlCommand, store, [hash]);
  const filePath = paths.get(hash);
  if (filePath === undefined) throw new Error(`${named}: sessions.sql gives no file of this hash`);
  let built: SessionProfile;
  try {
    built = await pulledProfile(plan.run, store, filePath, spool, skill);
  } catch (error) {
    throw new Error(`${named}: ${messageFor(error)}`);
  }
  if (!built.listed) {
    throw new Refused(
      `${named}: its rebuilt context (${built.profile.context}) does not list ${skill}, so a written task in it would test nothing; give a session whose context lists it`
    );
  }
  return { store, hash, pathHash: pathHash(filePath), built };
}

/** A task line as build-tasks writes it, with the profile `--profiles-out` gives it. */
interface TaskLine {
  readonly id: string;
  readonly source: "session" | "written";
  readonly profile?: string;
}

/** The session a task came from: its store, its file hash, and its path when it is known. */
interface TaskSession {
  readonly store: string;
  readonly hash: string | undefined;
  readonly path?: string;
}

/** The reason a session task, or a candidate session of one, is dropped when its rebuilt context
 *  does not list the tested skill (`listsSkill`). */
const SKILL_NOT_LISTED = "skill-not-listed";
/** The reason a candidate session is dropped when `--model` names another model than the one its
 *  first request ran on (`firstReplyModel`). */
const OTHER_MODEL = "other-model";

/**
 * Writes the profile of each task of `tasks` to `<plan.dir>/<task id>.json` (load.ts's
 * LoadProfile) and gives each task its path as `profile`: a task from a session (`sessions`, by
 * task id) the profile of that session, as `known` holds it (by task id) or rebuilt
 * (`pulledProfile`, pulled into `spool`), a written task the context session's. A task whose
 * profile cannot be rebuilt keeps no `profile`, and the record of it says why. A session task
 * whose profile does not list the skill (`listsSkill`) is dropped (`skill-not-listed`): it would
 * test nothing. Returns the tasks kept, the record of each task, and the sessions.sql queries it
 * made for the paths.
 */
async function writeProfiles<T extends TaskLine>(
  plan: ProfilesPlan,
  tasks: readonly T[],
  sessions: ReadonlyMap<string, TaskSession>,
  known: ReadonlyMap<string, SessionProfile>,
  context: ContextProfile | undefined,
  spool: string,
  skill: string,
  io: BuildIo
): Promise<{ tasks: T[]; notes: Record<string, unknown>[]; queries: number }> {
  mkdirSync(plan.dir, { recursive: true });
  const paths = new Map<string, string>();
  const wanted = new Map<string, string[]>();
  for (const [id, each] of sessions) {
    if (known.has(id) || each.path !== undefined || each.hash === undefined) continue;
    wanted.set(each.store, [...(wanted.get(each.store) ?? []), each.hash]);
  }
  let queries = 0;
  for (const [store, hashes] of wanted) {
    const resolved = await resolvePaths(plan.run.config.psqlCommand, store, hashes);
    queries += resolved.queries;
    for (const [hash, filePath] of resolved.paths) paths.set(`${store}\0${hash}`, filePath);
  }
  const notes: Record<string, unknown>[] = [];
  const out: T[] = [];
  for (const task of tasks) {
    let built: SessionProfile | undefined;
    let failed = "no --context-session gives a written task its profile";
    if (task.source === "written") built = context?.built;
    else {
      // A profile rebuilt already, when the session was a candidate, else one rebuilt now.
      built = known.get(task.id);
      const session = sessions.get(task.id);
      const filePath =
        session?.path ??
        (session?.hash === undefined ? undefined : paths.get(`${session.store}\0${session.hash}`));
      if (built === undefined && (session === undefined || session.hash === undefined)) {
        failed = "the message it came from records no file hash (an older build-tasks found it)";
      } else if (built === undefined && filePath === undefined) {
        failed = "sessions.sql gives no file of its hash";
      } else if (built === undefined && session !== undefined && filePath !== undefined) {
        try {
          built = await pulledProfile(plan.run, session.store, filePath, spool, skill);
        } catch (error) {
          failed = messageFor(error);
        }
      }
    }
    if (built === undefined) {
      notes.push({ task: task.id, from: task.source, profile: null, failed });
      io.err(`build-tasks: ${task.id}: no profile: ${failed}`);
      out.push(task);
      continue;
    }
    if (task.source === "session" && !built.listed) {
      notes.push({
        task: task.id,
        from: "session",
        profile: null,
        dropped: SKILL_NOT_LISTED,
        context: built.profile.context,
      });
      io.err(
        `build-tasks: ${task.id}: dropped (${SKILL_NOT_LISTED}): its context ${built.profile.context} does not list ${skill}`
      );
      continue;
    }
    const file = path.resolve(plan.dir, `${task.id}.json`);
    writeWhole(file, json(built.profile));
    notes.push({
      task: task.id,
      from: task.source === "written" ? "context-session" : "session",
      profile: file,
      context: built.profile.context,
      skills: built.profile.skills.length,
      ...(task.source === "written"
        ? {}
        : { secretsRemoved: built.secretsRemoved, evidence: built.evidence }),
    });
    out.push({ ...task, profile: file });
  }
  return { tasks: out, notes, queries };
}

/** What a summary records of a run's profiles: where they are, the context session (with the
 *  evidence of its profile), each task's, and the sessions.sql queries for their paths. */
function profilesRecord(
  plan: ProfilesPlan,
  context: ContextProfile | undefined,
  written: { notes: Record<string, unknown>[]; queries: number }
): Record<string, unknown> {
  return {
    dir: path.resolve(plan.dir),
    contextSession:
      context === undefined
        ? null
        : {
            store: context.store,
            hash: context.hash,
            pathHash: context.pathHash,
            context: context.built.profile.context,
            secretsRemoved: context.built.secretsRemoved,
            evidence: context.built.evidence,
          },
    tasks: written.notes,
    sessionQueries: written.queries,
  };
}

/** A candidate session the tasks reader is not offered, and why: its first request ran on another
 *  model than `--model` (`other-model`), its context does not list the skill
 *  (`skill-not-listed`), or it cannot be pulled or its profile rebuilt. */
const CandidateDrop = z.strictObject({
  store: z.string(),
  pathHash: z.string(),
  reason: z.string(),
  detail: z.string(),
  context: z.string().optional(),
});
type CandidateDrop = z.infer<typeof CandidateDrop>;

/** The bounds of build-tasks' candidate search, each null when not given: `--max-candidates`, the
 *  candidates decided (accepted or dropped), and `--max-search-minutes`, the search's own time;
 *  both summed over the runs of one `--out`. */
const SearchBounds = z.strictObject({
  maxCandidates: z.number().nullable(),
  maxSearchMinutes: z.number().nullable(),
});
type SearchBounds = z.infer<typeof SearchBounds>;

/** A bound of the candidate search, by its flag. */
const BOUNDS = ["--max-candidates", "--max-search-minutes"] as const;
type Bound = (typeof BOUNDS)[number];

/** What ended a candidate search: a bound, no candidate left, or TASK_MESSAGES accepted. */
type SearchEnd = Bound | "candidates ran out" | "enough offered";

/** A line of the search file (`<out>.candidates.jsonl`) after its first, which holds the bounds:
 *  a candidate accepted, a candidate dropped, or the bound that ended the search; each with the
 *  search's seconds then, summed over the runs. */
const SearchLine = z.union([
  z.strictObject({
    accepted: z.strictObject({ store: z.string(), pathHash: z.string() }),
    seconds: z.number(),
  }),
  z.strictObject({ dropped: CandidateDrop, seconds: z.number() }),
  z.strictObject({ ended: z.enum(BOUNDS), seconds: z.number() }),
]);
type SearchLine = z.infer<typeof SearchLine>;

/** What earlier runs of one `--out` saved of its candidate search: its lines in order; the
 *  decision of each candidate they decided, by `<store>\0<path hash>` (`candidateKey`): its drop,
 *  or null when it was accepted; the search's seconds at their last line; and the bound that
 *  ended the search, when one did. */
interface SavedSearch {
  readonly lines: readonly SearchLine[];
  readonly decided: ReadonlyMap<string, CandidateDrop | null>;
  readonly seconds: number;
  readonly ended: Bound | undefined;
}

/** Nothing saved of a candidate search. */
const NOTHING_SAVED: SavedSearch = { lines: [], decided: new Map(), seconds: 0, ended: undefined };

/** The candidate search of a build-tasks run that has bounds: the bounds, the search file each
 *  decision is saved in, and what earlier runs saved there. */
interface SearchPlan {
  readonly bounds: SearchBounds;
  readonly file: string;
  readonly saved: SavedSearch;
}

/** A candidate's key in a saved search. */
const candidateKey = (each: { readonly store: string; readonly pathHash: string }) =>
  `${each.store}\0${each.pathHash}`;

/** The bounds `--max-candidates` and `--max-search-minutes` of `values`, or undefined when
 *  neither is given. Refuses a value that is no bound, and a bound when the run makes no candidate
 *  search (`searches`: only `--model` or `--profiles-out` makes one). */
function searchBounds(
  values: Readonly<Record<string, unknown>>,
  searches: boolean
): SearchBounds | undefined {
  const candidates = values["max-candidates"];
  const minutes = values["max-search-minutes"];
  if (typeof candidates !== "string" && typeof minutes !== "string") return undefined;
  if (!searches) {
    throw new Refused(
      `--max-candidates and --max-search-minutes bound the candidate search, which runs only with --model or --profiles-out. ${BUILD_TASKS_USAGE}`
    );
  }
  if (typeof candidates === "string" && (!/^\d+$/.test(candidates) || Number(candidates) < 1)) {
    throw new Refused(`--max-candidates ${candidates} is not a whole number of 1 or more`);
  }
  const m = Number(minutes);
  if (typeof minutes === "string" && (minutes.trim() === "" || !(Number.isFinite(m) && m > 0))) {
    throw new Refused(`--max-search-minutes ${minutes} is not a number of minutes above 0`);
  }
  return {
    maxCandidates: typeof candidates === "string" ? Number(candidates) : null,
    maxSearchMinutes: typeof minutes === "string" ? m : null,
  };
}

/** The bound of `bounds` a search has reached with `decided` candidates decided in `seconds` of
 *  its own time, or undefined. */
function boundReached(bounds: SearchBounds, decided: number, seconds: number): Bound | undefined {
  if (bounds.maxCandidates !== null && decided >= bounds.maxCandidates) return "--max-candidates";
  if (bounds.maxSearchMinutes !== null && seconds >= bounds.maxSearchMinutes * 60) {
    return "--max-search-minutes";
  }
  return undefined;
}

/**
 * What earlier runs saved in the search file `file`: the bounds of its first line, then its lines
 * up to the first that does not parse (one a kill cut short) or the line of the bound that ended
 * the search. Refuses (exit 2) a file that holds a line of the search made under other bounds than
 * `bounds` (undefined when none is given), naming both values of each bound that differs; a file
 * with no line after the bounds holds nothing, whatever its bounds.
 */
function savedSearch(file: string, bounds: SearchBounds | undefined): SavedSearch {
  if (!existsSync(file)) return NOTHING_SAVED;
  const [first = "", ...rest] = readFileSync(file, "utf8").split("\n");
  let was: SearchBounds;
  try {
    was = z.strictObject({ bounds: SearchBounds }).parse(JSON.parse(first)).bounds;
  } catch {
    return NOTHING_SAVED;
  }
  const lines: SearchLine[] = [];
  for (const text of rest) {
    let line: SearchLine;
    try {
      line = SearchLine.parse(JSON.parse(text));
    } catch {
      break;
    }
    lines.push(line);
    if ("ended" in line) break;
  }
  const last = lines[lines.length - 1];
  if (last === undefined) return NOTHING_SAVED;
  const decided = new Map<string, CandidateDrop | null>();
  for (const line of lines) {
    if ("accepted" in line) decided.set(candidateKey(line.accepted), null);
    else if ("dropped" in line) decided.set(candidateKey(line.dropped), line.dropped);
  }
  const now = bounds ?? { maxCandidates: null, maxSearchMinutes: null };
  const given = (value: number | null) => (value === null ? "not given" : String(value));
  const differ: string[] = [];
  if (was.maxCandidates !== now.maxCandidates) {
    differ.push(
      `--max-candidates was ${given(was.maxCandidates)}, now ${given(now.maxCandidates)}`
    );
  }
  if (was.maxSearchMinutes !== now.maxSearchMinutes) {
    differ.push(
      `--max-search-minutes was ${given(was.maxSearchMinutes)}, now ${given(now.maxSearchMinutes)}`
    );
  }
  if (differ.length > 0) {
    throw new Refused(
      `${file} holds a candidate search made under other bounds (${decided.size} candidates decided), so this run does not go on from it: ${differ.join("; ")}`
    );
  }
  return {
    lines,
    decided,
    seconds: last.seconds,
    ended: "ended" in last ? last.ended : undefined,
  };
}

/** A bounded search's record in the summary (`search`). */
type SearchRecord = SearchBounds & {
  readonly ended: SearchEnd;
  readonly decided: number;
  readonly accepted: number;
  readonly seconds: number;
};

/**
 * The first user messages the tasks reader is offered: the first message of each distinct text,
 * at most TASK_MESSAGES spread evenly over them (`spread`). With `--model` (`model`, in the models
 * table's form) or `--profiles-out` (`plan`), each offered message's session is pulled: a message
 * is offered only when its first request ran on `model`
 * (`firstReplyModel`), and, with `plan`, its session is rebuilt (`sessionProfile`) and the message
 * offered only when its context lists the skill (`listsSkill`). One on another model, one whose
 * context does not list the skill, or one that cannot be pulled or rebuilt, is dropped and
 * counted, and the next session of the same text is tried, then the next text, until TASK_MESSAGES
 * are offered or the candidates run out (the reader then writes tasks, in the `--context-session`
 * context).
 *
 * With `search` (the run's bounds), the search also ends before its next candidate once the
 * candidates decided reach `--max-candidates`, or its own time (from the start of this function,
 * on `io.now`) reaches `--max-search-minutes`, both summed over the runs of the same `--out`; the
 * time is checked between candidates, so the last one decided can run past it. Each decision is
 * appended to the search file as one whole line as it is made, and the bound that ends the search
 * too. A run that goes on takes each candidate the file decides as it was decided, with no pull,
 * and ends where the file says a bound ended the search; a candidate a kill cut short is decided
 * again, and only the time of the run that decides it counts.
 *
 * Returns the messages, how many distinct texts there were, the profiles rebuilt (by
 * `<store>\0<hash>`), the drops, the sessions.sql queries made for the paths, and, with `search`,
 * the search's record.
 */
async function offeredMessages(
  found: readonly FoundRow[],
  plan: ProfilesPlan | undefined,
  model: string | undefined,
  search: SearchPlan | undefined,
  config: SkillCutsConfig,
  spool: string,
  skill: string,
  io: BuildIo
): Promise<{
  offered: (FoundRow & { id: string })[];
  distinct: number;
  built: Map<string, SessionProfile>;
  drops: CandidateDrop[];
  queries: number;
  search?: SearchRecord;
}> {
  const groups = new Map<string, FoundRow[]>();
  for (const row of found) {
    const key = row.text.trim();
    if (key !== "") groups.set(key, [...(groups.get(key) ?? []), row]);
  }
  const firsts = [...groups.values()].flatMap((rows) => rows.slice(0, 1));
  const chosen = spread(firsts, TASK_MESSAGES);
  const numbered = (rows: readonly FoundRow[]) =>
    rows.map((row, n) => ({ id: `m${n + 1}`, ...row }));
  const built = new Map<string, SessionProfile>();
  const drops: CandidateDrop[] = [];
  if (plan === undefined && model === undefined) {
    return { offered: numbered(chosen), distinct: groups.size, built, drops, queries: 0 };
  }
  const started = io.now();
  const saved = search?.saved ?? NOTHING_SAVED;
  const seconds = () => Math.round(saved.seconds * 1000 + io.now() - started) / 1000;
  const save = (line: SearchLine) => {
    if (search !== undefined) appendFileSync(search.file, `${JSON.stringify(line)}\n`);
  };
  // The search file starts again from its bounds and the lines that parse, so a line a kill cut
  // short is not followed by the next.
  if (search !== undefined) {
    const lines = [{ bounds: search.bounds }, ...saved.lines];
    writeWhole(search.file, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
  }
  const paths = new Map<string, string>();
  let queries = 0;
  for (const store of new Set(found.map((row) => row.store))) {
    const hashes = [
      ...new Set(found.flatMap((row) => (row.store === store && row.hash ? [row.hash] : []))),
    ];
    if (hashes.length === 0) continue;
    const resolved = await resolvePaths(config.psqlCommand, store, hashes);
    queries += resolved.queries;
    for (const [hash, filePath] of resolved.paths) paths.set(`${store}\0${hash}`, filePath);
  }
  // Each session pulled, by `<store>\0<hash>`: the model its first request ran on, and its
  // profile when `plan` asks for one and the model is the one asked for.
  const pulled = new Map<
    string,
    { readonly model: string | undefined; readonly profile: SessionProfile | undefined }
  >();
  const accepted: FoundRow[] = [];
  const order = [...chosen, ...firsts.filter((row) => !chosen.includes(row))];
  let decided = 0;
  let ended: SearchEnd | undefined;
  for (const first of order) {
    if (accepted.length === TASK_MESSAGES || ended !== undefined) break;
    for (const row of groups.get(first.text.trim()) ?? []) {
      const earlier = saved.decided.get(candidateKey(row));
      if (earlier !== undefined) {
        decided += 1;
        if (earlier === null) {
          accepted.push(row);
          break;
        }
        drops.push(earlier);
        continue;
      }
      if (search !== undefined) {
        const bound = saved.ended ?? boundReached(search.bounds, decided, seconds());
        if (bound !== undefined) {
          if (saved.ended === undefined) save({ ended: bound, seconds: seconds() });
          ended = bound;
          break;
        }
      }
      decided += 1;
      const drop = (reason: string, detail: string, context?: string) => {
        const each: CandidateDrop = {
          store: row.store,
          pathHash: row.pathHash,
          reason,
          detail,
          ...(context === undefined ? {} : { context }),
        };
        drops.push(each);
        save({ dropped: each, seconds: seconds() });
        io.out(
          `build-tasks: candidate ${row.store} ${row.pathHash.slice(0, 12)}: dropped (${reason}) ${detail}\n`
        );
      };
      if (row.hash === undefined) {
        drop("no-file-hash", "the message records no file hash (an older build-tasks found it)");
        continue;
      }
      const key = `${row.store}\0${row.hash}`;
      let session = pulled.get(key);
      if (session === undefined) {
        const filePath = paths.get(key);
        if (filePath === undefined) {
          drop("not-in-store", "sessions.sql gives no file of its hash");
          continue;
        }
        try {
          const t = await pulledTranscript(config.psqlCommand, row.store, filePath, spool);
          const ran = model === undefined ? undefined : firstReplyModel(t);
          session = {
            model: ran,
            profile:
              plan === undefined || ran !== model
                ? undefined
                : await sessionProfile(plan.run, storeNamed(config, row.store), filePath, t, skill),
          };
        } catch (error) {
          drop(plan === undefined ? "pull-failed" : "profile-failed", messageFor(error));
          continue;
        }
        pulled.set(key, session);
        if (session.profile !== undefined) built.set(key, session.profile);
      }
      if (model !== undefined && session.model !== model) {
        drop(
          OTHER_MODEL,
          `its first reply ran on ${session.model ?? "no model (no model_change comes before it)"}, not ${model}`
        );
        continue;
      }
      if (session.profile !== undefined && !session.profile.listed) {
        const { context } = session.profile.profile;
        drop(SKILL_NOT_LISTED, `its context ${context} does not list ${skill}`, context);
        continue;
      }
      if (search !== undefined) {
        save({ accepted: { store: row.store, pathHash: row.pathHash }, seconds: seconds() });
        io.out(`build-tasks: candidate ${row.store} ${row.pathHash.slice(0, 12)}: accepted\n`);
      }
      accepted.push(row);
      break;
    }
  }
  const offered = numbered(accepted);
  if (search === undefined) return { offered, distinct: groups.size, built, drops, queries };
  ended ??= accepted.length === TASK_MESSAGES ? "enough offered" : "candidates ran out";
  const record: SearchRecord = {
    ...search.bounds,
    ended,
    decided,
    accepted: accepted.length,
    seconds: seconds(),
  };
  io.out(
    `build-tasks: the candidate search ended (${ended}): ${decided} candidates decided, ${accepted.length} accepted, ${drops.length} dropped, ${record.seconds} s of search\n`
  );
  return { offered, distinct: groups.size, built, drops, queries, search: record };
}

/** The model `--model` names (`value`), in the models table's form (`modelForm`), or undefined
 *  when it names none. Refuses a blank one. */
function modelOption(value: unknown): string | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value.trim() === "") {
    throw new Refused(`--model names no model. ${BUILD_TASKS_USAGE}`);
  }
  return modelForm(value.trim());
}

/** `skill-cuts build-tasks`. */
async function buildTasks(argv: readonly string[], io: BuildIo): Promise<number> {
  const values = parse(
    argv,
    {
      config: { type: "string" },
      "key-file": { type: "string" },
      skill: { type: "string" },
      store: { type: "string", multiple: true },
      out: { type: "string" },
      "first-read": { type: "boolean" },
      sessions: { type: "string" },
      seed: { type: "string" },
      max: { type: "string" },
      "profiles-out": { type: "string" },
      copies: { type: "string" },
      count: { type: "string" },
      "context-session": { type: "string" },
      model: { type: "string" },
      "max-candidates": { type: "string" },
      "max-search-minutes": { type: "string" },
    },
    BUILD_TASKS_USAGE
  );
  if (values["first-read"] === true) return buildControlTasks(values, io);
  for (const flag of ["sessions", "seed", "max"] as const) {
    if (values[flag] !== undefined) {
      throw new Refused(`--${flag} goes with --first-read only. ${BUILD_TASKS_USAGE}`);
    }
  }
  const given = required(values, ["config", "key-file", "skill", "out"], BUILD_TASKS_USAGE);
  const stores = values.store ?? [];
  if (stores.length === 0) throw new Refused(`--store required. ${BUILD_TASKS_USAGE}`);
  const config = configFrom(given.config);
  for (const store of stores) {
    try {
      storeNamed(config, store);
    } catch (error) {
      throw new Refused(messageFor(error));
    }
  }
  mustBeFile(given["key-file"], "--key-file");
  const skill = taskSkill(given.skill);
  const model = modelOption(values.model);
  const plan = profilesPlan(values, config, io, true);
  const bounds = searchBounds(values, plan !== undefined || model !== undefined);
  // An earlier run of the same inputs at --out: its terms and every query it recorded are kept,
  // with the messages those queries found (`<out>.found.jsonl`), and the build goes on from the
  // first query it did not record. A finished build is left as it is.
  const summaryFile = `${given.out}.summary.json`;
  const foundFile = `${given.out}.found.jsonl`;
  const inputs: TasksInputs = {
    skill: digestOf(given.skill, ["SKILL.md"]),
    stores,
    window: { start: config.windowStart, end: config.windowEnd },
    ...(model === undefined ? {} : { model }),
  };
  const prior = priorTasks(given.out, summaryFile, inputs);
  if (prior?.done === true) {
    io.out(`build-tasks: ${summaryFile} records a finished build of these inputs; nothing to do\n`);
    return EXIT.ok;
  }
  // The candidate search an earlier run of the same inputs saved (`<out>.candidates.jsonl`), which
  // a build with no summary at --out does not go on from.
  const searchFile = `${given.out}.candidates.jsonl`;
  const saved = prior === undefined ? NOTHING_SAVED : savedSearch(searchFile, bounds);
  if (saved.lines.length > 0) {
    io.out(
      `build-tasks: going on from ${saved.decided.size} candidates decided in ${searchFile}\n`
    );
  }
  const priorSpend = prior?.spend ?? NO_SPEND;
  const recorded = new Map(
    (prior?.queries ?? []).map((query) => [`${query.store}\0${query.day}`, query])
  );
  const found: FoundRow[] = [];
  if (existsSync(foundFile)) {
    for (const line of readFileSync(foundFile, "utf8").split("\n")) {
      let row: FoundRow;
      try {
        row = FoundRow.parse(JSON.parse(line));
      } catch {
        continue;
      }
      if (typeof recorded.get(`${row.store}\0${row.day}`)?.rows === "number") found.push(row);
    }
  }
  writeWhole(foundFile, found.map((row) => `${JSON.stringify(row)}\n`).join(""));
  // The profile written tasks take, rebuilt before any reader call: a session that gives none
  // stops the build here. With --model or --profiles-out each candidate session is pulled too.
  const spools = plan === undefined && model === undefined ? undefined : spoolDir(given.out);
  const spool = spools === undefined ? "" : path.join(spools, "profile.jsonl");
  const context = plan === undefined ? undefined : await contextProfile(plan, spool, skill.name);
  const gateway = readerGateway(config, given["key-file"]);
  const spend = () => addSpend(priorSpend, gateway.spend());
  let terms = prior?.terms;
  const queries: TasksQuery[] = [];
  const progress = () =>
    writeWhole(
      summaryFile,
      json({
        command: "build-tasks",
        skill: skill.name,
        inputs,
        done: false,
        terms: terms ?? null,
        queries,
        readerSpend: spend(),
      })
    );

  if (terms === undefined) {
    const proposal = await runReader(
      "tasks",
      { skill, messages: [] } satisfies TasksInput,
      gateway
    );
    terms = TasksAnswer.parse(proposal.answer).terms;
    progress();
  }
  const searched = terms;
  io.out(`build-tasks: terms ${searched.map((term) => JSON.stringify(term)).join(", ")}\n`);

  for (const store of stores) {
    for (const day of windowDays(config.windowStart, config.windowEnd)) {
      const done = recorded.get(`${store}\0${day}`);
      if (done !== undefined) {
        queries.push(done);
        continue;
      }
      let query: TasksQuery;
      try {
        const rows = await storeRows(
          config.psqlCommand,
          store,
          tasksSql(mtimeRange(day), config.windowStart, config.windowEnd, searched)
        );
        const lines = rows.map((each) => {
          const row = TaskRow.parse(each);
          return {
            store,
            day,
            pathHash: pathHash(row.path),
            hash: fileHash(row.path),
            text: row.text,
          };
        });
        // The query's messages are appended whole before the summary records the query; a run
        // that goes on from a kill between the two leaves them out and runs the query again.
        appendFileSync(foundFile, lines.map((row) => `${JSON.stringify(row)}\n`).join(""));
        found.push(...lines);
        query = { store, day, rows: rows.length };
      } catch (error) {
        query = { store, day, rows: null, error: messageFor(error) };
        io.err(`build-tasks: ${store} ${day}: ${messageFor(error)}`);
      }
      queries.push(query);
      progress();
      io.out(`build-tasks: ${store} ${day}: ${query.rows ?? "no"} rows\n`);
    }
  }
  const candidates = await offeredMessages(
    found,
    plan,
    model,
    bounds === undefined ? undefined : { bounds, file: searchFile, saved },
    config,
    spool,
    skill.name,
    io
  );
  const { offered } = candidates;
  const picked = await runReader(
    "tasks",
    { skill, messages: offered.map(({ id, text }) => ({ id, text })) } satisfies TasksInput,
    gateway
  );
  const answer = TasksAnswer.parse(picked.answer);
  // A need task whose first step the skill does not govern is left out, with the reader's reason:
  // an agent doing it would act before it read the skill, so its first reply is no load test.
  const kept = answer.tasks.filter(
    (task) => task.label !== "need" || task.firstStep?.governed === true
  );
  const dropped = answer.tasks
    .filter((task) => task.label === "need" && task.firstStep?.governed !== true)
    .map((task) => ({
      source: task.from,
      ...(task.from === "session" ? { messageId: task.messageId } : { text: task.text }),
      why: task.why,
      reason: task.firstStep?.reason ?? "",
    }));
  const byId = new Map(offered.map((message) => [message.id, message]));
  const tasks = kept.map((task, n) => {
    const id = `t${n + 1}`;
    if (task.from === "written")
      return { id, text: task.text, label: task.label, source: "written" as const };
    const message = byId.get(task.messageId);
    if (message === undefined)
      throw new Error(`the tasks reader names no message ${task.messageId}`);
    return {
      id,
      text: message.text,
      label: task.label,
      source: "session" as const,
      store: message.store,
      pathHash: message.pathHash,
    };
  });
  // Each task's profile, when asked for: a session task's the one its session was offered under,
  // a written task's the context session's.
  const sessions = new Map<string, TaskSession>();
  const known = new Map<string, SessionProfile>();
  for (const [n, task] of kept.entries()) {
    const message = task.from === "session" ? byId.get(task.messageId) : undefined;
    if (message === undefined) continue;
    sessions.set(`t${n + 1}`, { store: message.store, hash: message.hash });
    const built = candidates.built.get(`${message.store}\0${message.hash}`);
    if (built !== undefined) known.set(`t${n + 1}`, built);
  }
  const profiled =
    plan === undefined
      ? undefined
      : await writeProfiles(plan, tasks, sessions, known, context, spool, skill.name, io);
  if (spools !== undefined) rmSync(spools, { recursive: true, force: true });
  const lines = profiled?.tasks ?? tasks;
  writeWhole(given.out, lines.map((task) => `${JSON.stringify(task)}\n`).join(""));
  const count = (label: string, source: string) =>
    lines.filter((task) => task.label === label && task.source === source).length;
  writeWhole(
    summaryFile,
    json({
      command: "build-tasks",
      skill: skill.name,
      inputs,
      done: true,
      stores,
      window: { start: config.windowStart, end: config.windowEnd },
      ...(model === undefined ? {} : { model }),
      terms: searched,
      queries,
      messagesFound: found.length,
      messagesDistinct: candidates.distinct,
      messagesOffered: offered.length,
      tasks: kept.map((task, n) => ({
        id: `t${n + 1}`,
        label: task.label,
        source: task.from,
        why: task.why,
        ...(task.firstStep === undefined ? {} : { firstStep: task.firstStep }),
      })),
      droppedNeed: dropped.length,
      dropped,
      ...(plan === undefined && model === undefined
        ? {}
        : {
            candidateDrops: {
              counts: Object.fromEntries(
                [...new Set(candidates.drops.map((each) => each.reason))].map((reason) => [
                  reason,
                  candidates.drops.filter((each) => each.reason === reason).length,
                ])
              ),
              sessions: candidates.drops,
              sessionQueries: candidates.queries,
            },
          }),
      ...(candidates.search === undefined ? {} : { search: candidates.search }),
      counts: {
        need: { session: count("need", "session"), written: count("need", "written") },
        "near-miss": {
          session: count("near-miss", "session"),
          written: count("near-miss", "written"),
        },
      },
      ...(plan === undefined || profiled === undefined
        ? {}
        : { profiles: profilesRecord(plan, context, profiled) }),
      readerSpend: spend(),
    })
  );
  // The messages found are in the tasks file where a task names one; the rest are not kept. The
  // summary records the candidate search, so its file is not kept either.
  rmSync(foundFile, { force: true });
  rmSync(searchFile, { force: true });
  const notListed = candidates.drops.filter((each) => each.reason === SKILL_NOT_LISTED).length;
  const onOther = candidates.drops.filter((each) => each.reason === OTHER_MODEL).length;
  const drops =
    plan === undefined && model === undefined
      ? ""
      : `, ${candidates.drops.length} candidate sessions dropped (${[
          ...(plan === undefined ? [] : [`${notListed} ${SKILL_NOT_LISTED}`]),
          ...(model === undefined ? [] : [`${onOther} ${OTHER_MODEL}, not on ${model}`]),
        ].join(", ")})`;
  io.out(
    `build-tasks: ${lines.length} tasks (${lines.filter((task) => task.source === "session").length} from sessions) from ${offered.length} first user messages offered, ${dropped.length} need tasks left out whose first step the skill does not govern${drops}; $${spend().usd.toFixed(4)} of readers at list price; written to ${given.out} and ${summaryFile}\n`
  );
  return EXIT.ok;
}

/** What `build-tasks --first-read` is made from: the skill's SKILL.md, the sessions file, the seed,
 *  `--max`, and the model `--model` names, when it names one. */
const ControlInputs = z.strictObject({
  skill: Digested,
  sessions: Digested,
  seed: z.string(),
  max: z.number(),
  model: z.string().optional(),
});
type ControlInputs = z.infer<typeof ControlInputs>;

/** One session `build-tasks --first-read` tried, as its summary records it. `outcome` is
 *  `first request` when the session's first assistant message read the skill (`loadedBy` says
 *  how), else why its first request is no task of the control. */
interface ControlRecord {
  store: string;
  hash: string;
  pathHash?: string;
  pulls?: number;
  outcome: string;
  detail?: string;
  loadedBy?: string;
}

/** A first request the control found, as `<out>.found.jsonl` keeps it until the tasks are
 *  written: its session, the hash of its path, and its text with its secrets taken out. */
const ControlFound = z.strictObject({
  store: z.string(),
  hash: z.string(),
  pathHash: z.string().regex(/^[0-9a-f]{64}$/),
  text: z.string(),
  secretsRemoved: z.number().int().nonnegative(),
});
type ControlFound = z.infer<typeof ControlFound>;

/** What the tasks reader said of one first request: whether the skill governs its first step. */
const ControlJudged = z.strictObject({
  store: z.string(),
  hash: z.string(),
  governed: z.boolean(),
  reason: z.string(),
});
type ControlJudged = z.infer<typeof ControlJudged>;

/** A `build-tasks --first-read` summary as a run that goes on reads it back. */
const ControlSummary = z.looseObject({
  command: z.literal("build-tasks"),
  mode: z.literal("first-read"),
  inputs: ControlInputs,
  done: z.boolean(),
  sessions: z.array(z.looseObject({ store: z.string(), hash: z.string(), outcome: z.string() })),
  judged: z.array(ControlJudged),
  sessionQueries: z.number(),
  readerSpend: SpendRecord,
});

/** The outcome of a session whose first request is a task of the control. */
const FIRST_REQUEST = "first request";
/** The outcome of a session whose first request is the text of an earlier one's. */
const SAME_REQUEST = "same request";
/** The outcomes of a session whose first assistant message read the skill: its first request is a
 *  task, an earlier one's text, or one whose rebuilt context cannot be had or does not list the
 *  skill. */
const READ_FIRST: ReadonlySet<string> = new Set([
  FIRST_REQUEST,
  SAME_REQUEST,
  "profile-failed",
  SKILL_NOT_LISTED,
]);

/**
 * `build-tasks --first-read`: the load test's positive control's tasks. The
 * sessions of `--sessions` (`{"store", "hash", "path"?}`, the sessions file attribute.ts writes
 * for the skill) are taken in judge.ts's `blindOrder` under `--seed`, one at a time: each is
 * pulled, and kept when its first assistant message loads the skill by load.ts's rule
 * (`firstMessageLoad`: an Oh My Pi `read` of `skill://<name>` or its SKILL.md; the stores hold
 * Oh My Pi transcripts). Its first user message, its secrets taken out, is a need task, unchosen,
 * unless an earlier session's first request is the same text. The tasks reader, in its `control`
 * mode, says of each such request whether the skill governs its first step; one it does not is
 * left out and counted, with the reader's reason. The build stops once `--max` requests
 * (TASKS_PER_LABEL unless given) are kept, or the sessions run out. With `--model`, a session
 * whose first request ran on another model (`firstReplyModel`) is dropped first (`other-model`),
 * and the summary's `model` says how many sessions tried on that model read the skill in their
 * first reply (`firstRead`), which the command prints too, and says when it is under
 * TASKS_PER_LABEL. A run of the same inputs at `--out` goes on as `build-tasks` does: every
 * session it tried and every request the reader judged is kept, with no pull and no reader call.
 */
async function buildControlTasks(
  values: Readonly<Record<string, string | boolean | string[] | undefined>>,
  io: BuildIo
): Promise<number> {
  const given = required(
    values,
    ["config", "key-file", "skill", "sessions", "seed", "out"],
    BUILD_TASKS_USAGE
  );
  if (values.store !== undefined) {
    throw new Refused("--first-read takes its sessions from --sessions, so it takes no --store");
  }
  for (const flag of ["max-candidates", "max-search-minutes"]) {
    if (values[flag] !== undefined) {
      throw new Refused(`--first-read takes no --${flag}: it stops once --max requests are kept`);
    }
  }
  if (!/^\d+$/.test(given.seed)) throw new Refused(`--seed ${given.seed} is not a whole number`);
  const maxText = values.max;
  const max = typeof maxText === "string" ? Number(maxText) : TASKS_PER_LABEL;
  if (typeof maxText === "string" && (!/^\d+$/.test(maxText) || max < 1)) {
    throw new Refused(`--max ${maxText} is not a whole number of 1 or more`);
  }
  const config = configFrom(given.config);
  mustBeFile(given["key-file"], "--key-file");
  const skill = taskSkill(given.skill);
  const model = modelOption(values.model);
  const plan = profilesPlan(values, config, io, false);
  const candidates = readCandidates(given.sessions, config);
  const order = blindOrder(candidates, given.seed).map(({ item }) => item);
  const keyOf = (each: { store: string; hash: string }) => `${each.store}\0${each.hash}`;

  const summaryFile = `${given.out}.summary.json`;
  const foundFile = `${given.out}.found.jsonl`;
  const inputs: ControlInputs = {
    skill: digestOf(given.skill, ["SKILL.md"]),
    sessions: {
      ...digestOf(path.dirname(given.sessions), [path.basename(given.sessions)]),
      path: given.sessions,
    },
    seed: given.seed,
    max,
    ...(model === undefined ? {} : { model }),
  };

  // An earlier run of the same inputs: its sessions, its judgements and the requests it found.
  const recorded = new Map<string, ControlRecord>();
  const judged = new Map<string, ControlJudged>();
  let priorSpend: Spend = NO_SPEND;
  let sessionQueries = 0;
  if (existsSync(summaryFile)) {
    const parsed = ControlSummary.safeParse(readJson(summaryFile));
    if (!parsed.success) {
      throw new Refused(
        `${summaryFile} is no build-tasks --first-read summary, so this build does not go on from it; give another --out`
      );
    }
    const was = parsed.data.inputs;
    const differ: string[] = [];
    if (was.seed !== inputs.seed) differ.push(`--seed was ${was.seed}, now ${inputs.seed}`);
    if (was.max !== inputs.max) differ.push(`--max was ${was.max}, now ${inputs.max}`);
    if (was.model !== inputs.model) {
      differ.push(`--model was ${was.model ?? "not given"}, now ${inputs.model ?? "not given"}`);
    }
    for (const key of ["skill", "sessions"] as const) {
      if (was[key].sha256 !== inputs[key].sha256) {
        differ.push(`--${key} was ${was[key].path}, now ${inputs[key].path}, and it differs`);
      }
    }
    if (differ.length > 0) {
      throw new Refused(
        `--out ${given.out} holds a build of other inputs, so this one does not go on from it: ${differ.join("; ")}`
      );
    }
    if (parsed.data.done) {
      io.out(
        `build-tasks: ${summaryFile} records a finished build of these inputs; nothing to do\n`
      );
      return EXIT.ok;
    }
    for (const session of parsed.data.sessions) {
      recorded.set(keyOf(session), session as unknown as ControlRecord);
    }
    for (const each of parsed.data.judged) judged.set(keyOf(each), each);
    priorSpend = parsed.data.readerSpend;
    sessionQueries = parsed.data.sessionQueries;
  } else if (existsSync(given.out)) {
    throw new Refused(
      `--out ${given.out} is there but ${summaryFile} is not, so what built it is not known; give another --out`
    );
  }
  const found = new Map<string, ControlFound>();
  if (existsSync(foundFile)) {
    for (const line of readFileSync(foundFile, "utf8").split("\n")) {
      let row: ControlFound;
      try {
        row = ControlFound.parse(JSON.parse(line));
      } catch {
        continue;
      }
      if (recorded.get(keyOf(row))?.outcome === FIRST_REQUEST) found.set(keyOf(row), row);
    }
  }
  writeWhole(foundFile, [...found.values()].map((row) => `${JSON.stringify(row)}\n`).join(""));

  // Paths for the candidates that lack one and are not tried yet, per store.
  const paths = new Map<string, string>();
  for (const store of config.stores) {
    const hashes = order
      .filter(
        (each) => each.store === store.name && each.path === undefined && !recorded.has(keyOf(each))
      )
      .map((each) => each.hash);
    if (hashes.length === 0) continue;
    const resolved = await resolvePaths(config.psqlCommand, store.name, hashes);
    sessionQueries += resolved.queries;
    for (const [hash, filePath] of resolved.paths) paths.set(`${store.name}\0${hash}`, filePath);
  }

  // Each session is pulled into one spool file, read from there, and the file is pulled over by
  // the next session.
  const spools = spoolDir(given.out);
  const spool = path.join(spools, "session.jsonl");
  const gateway = readerGateway(config, given["key-file"]);
  const spend = () => addSpend(priorSpend, gateway.spend());
  const sessions: ControlRecord[] = [];
  const write = (done: boolean, extra: Record<string, unknown> = {}) =>
    writeWhole(
      summaryFile,
      json({
        command: "build-tasks",
        mode: "first-read",
        skill: skill.name,
        inputs,
        done,
        order: order.map(({ store, hash }) => ({ store, hash })),
        sessions,
        judged: [...judged.values()],
        sessionQueries,
        pullCalls: sessions.reduce((total, each) => total + (each.pulls ?? 0), 0),
        ...extra,
        readerSpend: spend(),
      })
    );

  /** The record of a session not tried yet, and its first request when it is a task. With
   *  `--model`, a session whose first request ran on another model is dropped (`other-model`).
   *  With `--profiles-out`, its profile is rebuilt from the spool it was pulled into (`built`, by
   *  `<store>\0<hash>`), and a session whose context does not list the skill is dropped
   *  (`skill-not-listed`), since its task would test nothing. */
  const texts = new Set([...found.values()].map((row) => row.text.trim()));
  const built = new Map<string, SessionProfile>();
  const tryOne = async (
    candidate: (typeof order)[number]
  ): Promise<{ record: ControlRecord; request?: ControlFound }> => {
    const record: ControlRecord = { store: candidate.store, hash: candidate.hash, outcome: "" };
    const no = (outcome: string, detail: string) => {
      record.outcome = outcome;
      record.detail = detail;
      return { record };
    };
    const transcriptPath = candidate.path ?? paths.get(keyOf(candidate));
    if (transcriptPath === undefined)
      return no("not-in-store", "sessions.sql gave no row of this hash");
    const hashOfPath = pathHash(transcriptPath);
    record.pathHash = hashOfPath;
    let pulled: { bytes: number; calls: number };
    try {
      pulled = await pullTranscript(config.psqlCommand, candidate.store, transcriptPath, spool);
    } catch (error) {
      return no("pull-failed", messageFor(error));
    }
    record.pulls = pulled.calls;
    let t: Transcript;
    try {
      t = readTranscript(spool);
    } catch (error) {
      return no("unparseable", messageFor(error));
    }
    if (model !== undefined) {
      const ran = firstReplyModel(t);
      if (ran !== model) {
        return no(
          OTHER_MODEL,
          `its first reply ran on ${ran ?? "no model (no model_change comes before it)"}, not ${model}`
        );
      }
    }
    const userAt = t.items.findIndex((item) => item.slim.role === "user");
    const replyAt = t.items.findIndex((item) => item.slim.role === "assistant");
    const request = userAt < 0 ? undefined : t.message(userAt);
    const reply = replyAt < 0 ? undefined : t.message(replyAt);
    const text = request === undefined ? "" : messageText(request);
    if (request === undefined || reply === undefined || text.trim() === "") {
      return no("no first request", "the session has no user message with text, or no reply");
    }
    // load.ts's rule, on the transcript's tool calls as a reply's `tool_use` blocks.
    const blocks = (reply.content ?? []).flatMap((block) =>
      block.type === "toolCall"
        ? [{ type: "tool_use", id: block.id, name: block.name, input: block.arguments }]
        : []
    );
    const load = firstMessageLoad("omp", blocks, { name: skill.name });
    if (!load.loaded) {
      return no(
        "not first",
        `its first assistant message reads no file of ${skill.name}: its tool calls are ${load.toolCalls.join(", ") || "none"}`
      );
    }
    const scrubbedText = removeSecrets(text);
    if (texts.has(scrubbedText.text.trim())) {
      return no(SAME_REQUEST, "an earlier session's first request is the same text");
    }
    if (plan !== undefined) {
      let profile: SessionProfile;
      try {
        profile = await sessionProfile(
          plan.run,
          storeNamed(config, candidate.store),
          transcriptPath,
          t,
          skill.name
        );
      } catch (error) {
        return no("profile-failed", messageFor(error));
      }
      if (!profile.listed) {
        return no(
          SKILL_NOT_LISTED,
          `its context ${profile.profile.context} does not list ${skill.name}`
        );
      }
      built.set(keyOf(candidate), profile);
    }
    texts.add(scrubbedText.text.trim());
    record.outcome = FIRST_REQUEST;
    record.loadedBy = load.loadedBy ?? undefined;
    return {
      record,
      request: {
        store: candidate.store,
        hash: candidate.hash,
        pathHash: hashOfPath,
        text: scrubbedText.text,
        secretsRemoved: scrubbedText.removed,
      },
    };
  };

  // The tasks reader, in its control mode, on the requests found and not judged yet.
  const countKept = () => [...judged.values()].filter((each) => each.governed).length;
  let pending: ControlFound[] = [];
  const judge = async () => {
    if (pending.length === 0) return;
    const batch = pending;
    pending = [];
    const result = await runReader(
      "tasks",
      {
        skill,
        messages: batch.map((row, n) => ({ id: `m${n + 1}`, text: row.text })),
        mode: "control",
      } satisfies TasksInput,
      gateway
    );
    const answer = TasksAnswer.parse(result.answer);
    for (const task of answer.tasks) {
      if (task.from !== "session") continue;
      const row = batch[Number(task.messageId.slice(1)) - 1];
      if (row === undefined) continue;
      judged.set(keyOf(row), {
        store: row.store,
        hash: row.hash,
        governed: task.firstStep?.governed === true,
        reason: task.firstStep?.reason ?? "",
      });
    }
    write(false);
  };

  for (const [position, candidate] of order.entries()) {
    if (countKept() + pending.length >= max) {
      await judge();
      if (countKept() >= max) break;
    }
    const key = keyOf(candidate);
    let record = recorded.get(key);
    if (record === undefined) {
      const tried = await tryOne(candidate);
      record = tried.record;
      if (tried.request !== undefined) {
        // The request is appended whole before the summary records its session.
        appendFileSync(foundFile, `${JSON.stringify(tried.request)}\n`);
        found.set(key, tried.request);
      }
      sessions.push(record);
      write(false);
      io.out(
        `build-tasks: ${position + 1}/${order.length} ${candidate.store} ${candidate.hash}: ${record.outcome}${record.detail === undefined ? "" : ` (${record.detail})`}\n`
      );
    } else {
      sessions.push(record);
    }
    const request = found.get(key);
    if (record.outcome === FIRST_REQUEST && request !== undefined && !judged.has(key)) {
      pending.push(request);
    }
  }
  if (countKept() < max) await judge();

  // The tasks: each request the reader says the skill governs the first step of, in the order.
  const kept: ControlFound[] = [];
  const dropped: { store: string; pathHash: string; reason: string }[] = [];
  for (const session of sessions) {
    const verdict = judged.get(keyOf(session));
    const request = found.get(keyOf(session));
    if (verdict === undefined || request === undefined) continue;
    if (verdict.governed && kept.length < max) kept.push(request);
    else if (!verdict.governed) {
      dropped.push({ store: request.store, pathHash: request.pathHash, reason: verdict.reason });
    }
  }
  const tasks = kept.map((row, n) => ({
    id: `t${n + 1}`,
    text: row.text,
    label: "need" as const,
    source: "session" as const,
    store: row.store,
    pathHash: row.pathHash,
  }));
  // Each task's profile, when asked for: the one its session was rebuilt with when it was tried,
  // else rebuilt now (a session an earlier run tried).
  const sessionsOf = new Map<string, TaskSession>();
  const known = new Map<string, SessionProfile>();
  for (const [n, row] of kept.entries()) {
    const candidate = order.find((each) => keyOf(each) === keyOf(row));
    sessionsOf.set(`t${n + 1}`, {
      store: row.store,
      hash: row.hash,
      path: candidate?.path ?? paths.get(keyOf(row)),
    });
    const profile = built.get(keyOf(row));
    if (profile !== undefined) known.set(`t${n + 1}`, profile);
  }
  const profiled =
    plan === undefined
      ? undefined
      : await writeProfiles(plan, tasks, sessionsOf, known, undefined, spool, skill.name, io);
  const lines = profiled?.tasks ?? tasks;
  writeWhole(given.out, lines.map((task) => `${JSON.stringify(task)}\n`).join(""));
  // With --model: how many sessions tried on it read the skill in their first reply, and how many
  // were on another model.
  const onModel =
    model === undefined
      ? undefined
      : {
          name: model,
          firstRead: sessions.filter((each) => READ_FIRST.has(each.outcome)).length,
          otherModel: sessions.filter((each) => each.outcome === OTHER_MODEL).length,
        };
  write(true, {
    tasks: kept.map((row, n) => ({
      id: `t${n + 1}`,
      store: row.store,
      pathHash: row.pathHash,
      secretsRemoved: row.secretsRemoved,
    })),
    droppedNeed: dropped.length,
    dropped,
    skillNotListed: sessions.filter((each) => each.outcome === SKILL_NOT_LISTED).length,
    ...(onModel === undefined ? {} : { model: onModel }),
    ...(plan === undefined || profiled === undefined
      ? {}
      : { profiles: profilesRecord(plan, undefined, profiled) }),
  });
  rmSync(foundFile, { force: true });
  rmSync(spools, { recursive: true, force: true });
  io.out(
    `build-tasks: ${lines.length} control tasks from ${sessions.length} sessions tried, ${dropped.length} left out whose first step the skill does not govern, ${sessions.filter((each) => each.outcome === SKILL_NOT_LISTED).length} whose context does not list the skill; $${spend().usd.toFixed(4)} of readers at list price; written to ${given.out} and ${summaryFile}\n`
  );
  if (onModel !== undefined) {
    io.out(
      `build-tasks: ${onModel.firstRead} sessions tried on ${onModel.name} read ${skill.name} in their first reply; ${onModel.otherModel} sessions on another model dropped (${OTHER_MODEL})\n`
    );
    if (onModel.firstRead < TASKS_PER_LABEL) {
      io.err(
        `build-tasks: ${onModel.firstRead} sessions on ${onModel.name} read ${skill.name} in their first reply, fewer than ${TASKS_PER_LABEL}`
      );
    }
  }
  if (tasks.length < max) {
    io.err(`build-tasks: the sessions gave ${tasks.length} control tasks, fewer than --max ${max}`);
  }
  return EXIT.ok;
}

// ---------------------------------------------------------------------------------------------
// The command

const USAGE = [BUILD_CASES_USAGE, BUILD_TASKS_USAGE].join("\n");

/** `skill-cuts build-cases …` and `skill-cuts build-tasks …`: `argv` starts with the command's
 *  name. Returns the exit status (EXIT). */
export async function main(argv: readonly string[], io: BuildIo): Promise<number> {
  const [command, ...rest] = argv;
  try {
    switch (command) {
      case "build-cases":
        return await buildCases(rest, io);
      case "build-tasks":
        return await buildTasks(rest, io);
      default:
        throw new Refused(`no command ${JSON.stringify(command ?? "")}.\n${USAGE}`);
    }
  } catch (error) {
    if (error instanceof Refused) {
      io.err(`${command ?? "build"}: ${error.message}`);
      return EXIT.refused;
    }
    io.err(`${command}: ${messageFor(error)}`);
    return EXIT.failed;
  }
}

if (import.meta.main) {
  process.exit(
    await main(Bun.argv.slice(2), {
      out: (text) => process.stdout.write(text),
      err: (line) => process.stderr.write(`${line}\n`),
      gh: spawnGh,
      now: () => performance.now(),
    })
  );
}
