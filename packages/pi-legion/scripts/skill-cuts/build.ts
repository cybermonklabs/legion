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
 *       --cut <id> --skill <name> --old <skill dir at the cut's base> [--new <skill dir at the
 *       cut's head>] --sessions <file> --seed <n> --copies <copies out dir> --count <count dir>
 *       --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>]
 *       [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]
 *   bun scripts/skill-cuts/build.ts build-cases --config <skill-cuts.json> --old <skill dir at
 *       the cut's base> --copies <copies out dir> --count <count dir> --reprofile <cases.jsonl>
 *       --out <new cases.jsonl>
 *   bun scripts/skill-cuts/build.ts build-tasks --config <skill-cuts.json> --key-file <file>
 *       --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir>
 *       --copies <copies out dir> --count <count dir> --context-session <store>:<hash>]
 *       [--max-candidates <n>] [--max-search-minutes <m>]
 *   bun scripts/skill-cuts/build.ts build-tasks --config <skill-cuts.json> --copies <copies out dir>
 *       --count <count dir> --reprofile <tasks.jsonl> --profiles-out <new dir>
 *       [--context-session <store>:<hash>]
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
 * SHA-256 of `--old`'s files, every file of `--new`, the sessions file, the copies files read and
 * the count's query outputs. When `--out` and its summary are there from a run of the same
 * inputs, the build goes on from them: every session they decided (dropped, or a case the cases
 * file holds) is kept as it was, with no pull and no reader call, and the build goes on in the
 * same order from the first session not decided, until `--max` cases in all; the budget counts
 * the dollars already spent. Inputs that differ are refused (exit 2), naming each; so is a cases
 * file with no summary, and a summary with no inputs (or with no `--new`, which an older
 * build-cases did not take). Each case is appended to the cases file as one whole line and then the
 * summary is written to a temporary name and renamed, so both stay readable after a kill at any
 * moment; a case line the summary does not record (a kill between the two) is left out when the
 * build goes on, and its session is read again. For each session:
 *
 * 1. Its transcript is pulled by path with pull.sql, PULL_PAGE slices a call (each 1 MiB and at
 *    most 3 bytes more), paging by place, into a file of its own (`<out>.spool/`, removed once
 *    the session is decided), each slice written as its row comes, read by psql-rows.ts's
 *    `PsqlTable` and checked by its `PullCheck`; so one row is held at a time, however long the
 *    file. It is read from there line by line (`readTranscript`), each line parsed as
 *    transcript.ts's entry; the text after the last newline, a line still being written, is left
 *    out when it does not parse. Only the entries the builder reads whole are held whole
 *    (`heldWhole`: the header, a `session_init`, model and thinking changes, context resets, a
 *    system prompt's digest, Envoy role claims); every other entry is held light, with where its
 *    line is, a message with its roles, tool calls and `[Skill file: …]` line, and is read whole
 *    again only where a check or a reader needs it (the reader's windows, the reads it offers, the
 *    case's messages, the compaction before the step). So a session holds its entries' light
 *    forms and what its case and windows need, not its file.
 * 2. The session's messages are those of its current branch (the parent chain of its last entry),
 *    in order, as Oh My Pi 18.18.6 gives them to the model: each `message` (less an assistant
 *    message it leaves out: one marked `retryRecovery`, or an error with no content), each
 *    `custom_message` as a `custom` message (less the two plan-mode kinds it leaves out), each
 *    `branch_summary` as a `branchSummary` message. A string `content` is made one text block.
 * 3. The session's reads of the skill are its reads of `skill://<skill>` files (as cases.ts's
 *    `skillReadFile` reads one) and its reads of the skill's files by path, by count.sql's rule
 *    (`readsByPath`: a path or URL that, its read selector taken off, ends `<skill>/SKILL.md` or
 *    holds `skills/<skill>/references/`). The copy a read reads is placed by attribute.ts's own
 *    rules (`placer`: the prefix table, then a checkout, then the session's Oh My Pi profile), from
 *    `--count` and `--copies`, the two attribute.ts made the sessions file from: a `skill://`
 *    read's at the path of its `[Skill file: <path>]` line, a read by path's at its own path, as
 *    read. A read inside a project skills directory of a checkout (`.omp/skills`, `.claude/skills`,
 *    `.agent/skills`, `.agents/skills`, `.codex/skills`, `.opencode/skills`, `.github/skills`, not
 *    a home directory's) is of a project skill, which they place only at a copy in such a
 *    directory, never at a unit's copy of its name, so such a read installs no plugin. A case is
 *    built from a successful `skill://` read, or from a successful whole read by
 *    path (no read selector) that they place at a copy of the cut's repository (a source). A
 *    source's directory is its path up to the skill's directory, resolved as Oh My Pi's read
 *    resolves it (cases.ts's `resolvedReadPath`): a relative path against the session's working
 *    directory, its header's `cwd`, and a path under `~` against the home directory it ran under,
 *    the part of its transcript's path before the first `/.omp/` (`sessionDirs`). The first
 *    source is found. A first `skill://` read is kept only when they place it at a copy of the
 *    cut's repository; one they place at another copy, or at none, is dropped with the rule
 *    named. A session with neither, whose successful reads of the skill are all reads by path of
 *    the kinds no case is built from (`pathKind`), is dropped by the first of their kinds in this
 *    order: one with a read selector (`path-range`), one of another repository's copy or a URL
 *    (`other-copy`), one at a path they place at no copy (`unattributed`), one of the cut's copy
 *    whose relative or `~` path needs a directory the session does not name, or that resolves to
 *    no absolute directory a case can name its copy by (`path-relative`); the drop's detail names
 *    each read and why.
 *
 *    Every check that needs no model's answer runs here, before the session's first reader call,
 *    so a session such a check drops costs no reader spend: the placement; the model and thinking
 *    level, when none the session sets from the read on can give its step a model, a level Oh My
 *    Pi takes, or a model the gateway serves (each the drop the step's own check makes); the
 *    session's role or subagent context; the lines a case may name (step 4); and the profile
 *    (below), built from every read the reader can reach, at each prompt build a step in its reach
 *    can run under: when no unit of those reads ships the cut repository's copy of the skill at any
 *    of them, no step's profile installs it. The
 *    checks of the step and its line (its model, the compaction and the reads of the skill's
 *    files before it, the form of each file read by path in the old and the new text, the
 *    line in the base text and in the read, a quote or restatement of it, and the profile of the
 *    reads before it) need the reader's answer, so they run after it and before the restatement
 *    check's and the audit's readers.
 * 4. The case reader (`skill-cuts read --prompt case`) is offered only the lines of the skill's
 *    base text (`--old`) that a case may name: those of each file that the session's own
 *    successful reads of that file hold whole (the text after its `[Skill file: …]` line, or a
 *    read by path's text less Oh My Pi's rows and its hashline line, `pathReadText`; each line
 *    trimmed, among the messages the reader reads), each with its number in the base file.
 *    A file the session did not read is not offered, and a session whose reads hold no line of
 *    the base text is dropped before any reader call (`no-common-line`), since the skill's text
 *    may have changed after the session read it. It reads them and the session's messages window
 *    by window, one call a window, from the read's call on: each window at most READER_MESSAGES
 *    messages and READER_CHARS characters, and before it, nearest first, at most
 *    READER_EARLIER_MESSAGES messages and READER_EARLIER_CHARS characters of those after the last
 *    compaction or context reset before the window. The next window starts after the last
 *    message of the one before, until the reader proposes a step, READER_WINDOWS windows are read
 *    (at most READER_WINDOWS × READER_MESSAGES messages after the read), or the messages end. No
 *    window reads past the first compaction or context reset after the skill's read: a step after
 *    it would follow a compaction that follows the read, which drops it, so a session whose
 *    windows reach it with no step is dropped for that (`read-before-compaction`). Each message is
 *    cut to MESSAGE_CHARS (its start and its end kept), and each read result of the skill's files
 *    (a `skill://` read, or a whole read by path of a source's copy) is left out with a note. The
 *    reader proposes the first step the skill governs, its message index, the line it exercises
 *    (one of those offered), the criterion, a violating step (a plausible next step that breaks
 *    the line and otherwise looks valid), and each message before the step that states the line's
 *    instruction in any wording; or no step. The session's record in the summary says how many
 *    windows the reader read (`windows`).
 * 5. The case: `messages` the model's messages before the step, `realStep` the real assistant
 *    message at it, `violatingStep` the reader's violating step as an assistant message (its text,
 *    then each tool call), `skillReads` the index of each read result of a `skill://<skill>` file
 *    in `messages` and, when a source before the step read the cut's copy by path, `pathCopy` (the
 *    first such source's resolved directory, with the session's `cwd` and `home` where known) and
 *    the index of each whole read by path of a file of that copy, its path resolved with those
 *    (cases.ts's `pathReadFile`), `model` and `thinking` the branch's last `model_change` and
 *    `thinking_level_change` before the step, `sessionDigest` its last `system-prompt-digest`
 *    before the step, `sessionStart` the header's timestamp, `source` the store and the SHA-256 of
 *    the path (never the path), `context` (`devbox`; `legion:<project>:<role>` from the session's
 *    role, below; or `subagent:<agent>`), and
 *    `profile` (below). Where a compaction precedes the step and every read of the skill follows
 *    it, `messages` is what Oh My Pi gives the model after it: the summary as a
 *    `compactionSummary` message, the entries it kept, and those after it. Each other successful
 *    read by path of the skill before the step (a selector, another copy, no copy, a path the
 *    build cannot resolve, or the cut's copy at another directory than `pathCopy`) is answered as
 *    recorded in every arm, and the case's `evidence` names each (EXCLUDED_READS), so the arms do
 *    not differ in part without saying so. Each file read whole by path is held to the form in
 *    which the arms answer a read by path (arms.ts's `pathFormFault`) in the old text, and in the
 *    new text when `--new` gives one; a case with one they do not answer is dropped (`path-form`),
 *    so no `behaviour` run fails on it. A build given no `--new` (no cut text yet) checks the old
 *    text alone, and `behaviour screen` drops a case the new text fails (cli.ts).
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
 * header; no read of a file of the skill succeeded, or each read by path that succeeded is of a
 * kind no case is built from (step 3); the first read is not the cut repository's copy; a
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
 * before the step, or there is none; a read before the step reads a file of the skill in a way
 * the case neither holds nor names (a `skill://` file with a selector, or a path that ends
 * `/<skill>/SKILL.md`, holds `skills/<skill>/` or is under the skill's installed directory and
 * is no read the case holds in `skillReads` or names as an excluded read: an error result of such
 * a read too); a file the case reads by path is one the arms do not answer in Oh My Pi's form; a
 * GitHub read the profile needs fails; or the profile's units install no copy of the skill from
 * the cut repository. A session the readers reach costs one case reader call a window, one
 * restatement call a window, and one audit call.
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
 * each unit's repositories at the time it is read, each part with its evidence in the case. That
 * time (the `read times` item) is a subagent's root's last prompt build before its start
 * (PARENT_LOAD_RULE); for a top-level session, the build its step ran under (TOP_BUILD_RULE): the
 * time of that build's `system-prompt-digest` line, from the session's own lines (the transcript,
 * or digest-times.sql for `--reprofile`) and the step's message time, or the session's start for
 * its first build. Below, "the session's start" means that time (a subagent's pools are read at
 * its own start):
 * - The context follows the store, the Oh My Pi profile and where the session's skills came from
 *   (SESSION_CONTEXT_RULE). A default-profile session in a devbox store uses the dev box's
 *   installers, and so does a named-profile one with a `[Skill file: …]` header under the
 *   configuration's `devboxHome`: a pod that mounts the dev box's home. Any other named profile in
 *   that store reads the session-pod image at the root's prompt build. A separate profile in a
 *   Legion store does not inherit Legion role plugins. The `context` label does not change.
 * - The units installed: each unit (`via`) of each prefix row that a `[Skill file: <path>]` of a
 *   read result before the step is under, in every context; outside a pod whose links decide its
 *   units, also the path of a successful whole read by path of the case's copy (`pathCopy`,
 *   `profileReads`); for a path no row maps, each unit of the prefix rows of the copy
 *   attribute.ts's rules place the read at (a checkout, the session's profile); and,
 *   for a Legion context, each Oh My Pi unit with a prefix row under `/opt/legion/` (a package
 *   the worker image carries) and each unit an `omp-plugins` manifest installs. A read no rule
 *   places is listed as left out; a copy a read is placed at that no prefix row names a unit for
 *   is installed alone. A read whose skill none of those units ships at the session's start (the
 *   copies output names a unit as the manifests name it later: a marketplace or plugin renamed
 *   since, whose old name the read's plugin cache directory shows) adds each unit of the prefix
 *   rows that the same manifests give at the session's start (copies.ts's rules, below) and that
 *   its path is under, and that unit's copy is the one the session read.
 * - Pools: a pool that an extension gives through `resources_discover` (a `pool:` unit of the
 *   dev box's `skills-sources.json`, which a read or the dev box's installers bring in) is decided
 *   by the recorded listing the listing measure sets the profile against (POOL_LISTING_RULE): the
 *   session's own recorded prompt's, or, for a session that records only its prompt's digest, that
 *   of the subagent the measure uses, which the build reads from the store (subagents.sql) before
 *   it builds the profile. The pool is installed where that listing names at least one of the
 *   skills it holds at the session's start that no installed unit outside the pools also ships;
 *   a name such a unit ships is no evidence for the pool, since the session lists it whether or
 *   not the pool is there, and the evidence names those names. The pool is left out where the
 *   listing names no such skill. A pool that a `[Skill file: …]` read of the session places is
 *   installed whatever the listing names: that read shows Oh My Pi loaded it, which is direct
 *   proof and no inference from the listing. A pool the listing names in part is installed whole:
 *   the evidence gives how many of its skills the listing names and how many it does not, and the
 *   listing measure counts those it does not as added names. The measure marks each name of a
 *   pool installed on the listing (`fromRecorded`), and no name of a pool installed on a read
 *   alone. The listing's decision is an inference: dev-box sessions that ran one Oh My Pi build
 *   list these pools in some sessions and not in others, and no file records why. A session with
 *   no listing (no recorded prompt, and no subagent the measure can use) has its pools decided by
 *   the Oh My Pi build its dev box pinned at its start (POOL_VERSION_RULE): the version the
 *   configuration's `ompPin` names (a TOML file of the dev box's repository and the keys to the
 *   version in it, read at the repository's last commit at or before the session's start), set
 *   against `ompPin`'s
 *   `firstPoolVersion`, the first build that loads such pools on a fresh start. Before that
 *   version, the profile installs no pool but one that a `[Skill file: …]` read of the session
 *   places, which shows Oh My Pi loaded it; at or after it, every pool. The Oh My Pi version the
 *   session's file records (the `runtime.version` of its first `model_change` line) is set beside
 *   the pin in the evidence as a cross-check, and decides nothing. Without `ompPin`, or where the
 *   pin file holds no version, such pools are installed as the dev box's installers install them.
 * - Commits: each repository at the last commit of its default branch (repositories.tsv, else
 *   GitHub's) at or before the session's start; a unit's repository that an installer of the
 *   units pins (copies.ts's `installerPins`: a `github:` dependency of an Oh My Pi plugins
 *   package.json, an `ensure_vendor` clone) at the last commit of the ref that installer names at
 *   the session's start, as the session's machine held it; for a Legion pod, the units of its
 *   worker image's repository at the commit the image was built from (LEGION_IMAGE_RULE). The
 *   deployment's project file (else its Pulumi config) names the image. Its commit is the one
 *   GitHub's build provenance of the image's digest names; else the one the labels of the image's
 *   own OCI config name (`org.opencontainers.image.revision`, else the release tag of
 *   `org.opencontainers.image.version`); else, for an image the configuration's
 *   `deploymentImages` names (one the deployment builds on a worker image), the commit of the
 *   base its Dockerfile at the image's revision names, once the base's `rootfs.diff_ids` are the
 *   first of the image's; else the one the pin history names (the comment lines right above the
 *   image's line); else the image's commit is not known, and the case or task is not measurable
 *   and is left out, since the repository's default branch would be a guess. The
 *   commit its daemon image was built from, at which a capture composes the role prompt, is an
 *   evidence item of its own (LEGION_DAEMON_RULE). A
 *   repository GitHub gives no commit of
 *   at or before that time (`NoCommitThen`), such as one the dev box's installers install only by
 *   the copies output's later read, did not exist then, so nothing of it was installed then: it is
 *   left out, with the `gh api` read in the evidence. A session repository with no commit then has
 *   its project skills and context files not read, and the profile names no session repository;
 *   no later commit is read in its place. Every GitHub read goes through `gh api`
 *   (io.gh): GET calls, and GraphQL queries that read a repository's files in batches. A read
 *   that fails for a transient reason is sent again by the rule copies.ts's and context.ts's reads
 *   share (common.ts's `GhRetries`: after 5, 15, 45 and 135 s, each retry said on standard error;
 *   a batch of more than one blob that GitHub answers as a batch too large is not sent again, and
 *   its blobs are read alone), and a read that failed so on every try is not kept, so the next
 *   case reads it again. A read GitHub refuses for its rate limit is sent again after a wait
 *   (`GithubReads.send` with common.ts's `rateLimitWait`: a primary limit waited out to the
 *   reset of the installation that refused it, the one of the owner the read names, at most 15
 *   minutes (`untilInstallationReset`); a secondary one SECONDARY_WAIT_MS,
 *   doubled at each refusal), at most MAX_RATE_RETRIES times; one still refused after them is a
 *   transient failure (`ghAnswer`), so it fails the profile as a read that fails so on every try
 *   does, and no SKILL.md it held is left out of the profile as a fault. The npm registry's
 *   tarball read (`GithubReads.tarball`, one fetch) gives
 *   up a try after 60 s (NPM_TIMEOUT_MS), its body included, and a try that failed for a
 *   transient reason (that limit, a connection refused or reset, a name not resolved, an HTTP 429
 *   or 5xx) is sent again by the same rule, counted with the GitHub reads' (`githubRetries`); a
 *   404 is no tarball, and any other failure fails at once. An image's config read
 *   (`GithubReads.imageConfig`, pod-image.ts's `registryImageConfig`) gives up each request (the
 *   token GHCR gives, a manifest, the config blob) after 30 s, its body included, and sends one
 *   that failed so again by the same rule, counted with them; a configured registry's 401 or 403
 *   is a refusal of its token and fails the read at once, naming the host, and a 404 is no image.
 * - Skills: what each unit ships at that commit, not what installers.tsv lists, which is what it
 *   ships at the later commit the copies output read. Each repository of a unit (its manifest's,
 *   and those of the copies installers.tsv lists for it) is read at its commit as copies.ts reads
 *   one: its tree, each `SKILL.md` outside the configuration's `excludePaths`, and each manifest
 *   file by its path; copies.ts's `resolveInstallers` then gives each unit's skills (the prepack
 *   script's list for a package, the skill directories Oh My Pi scans in an Oh My Pi package, a
 *   plugin's `skills/`, an installer's linked directory, a pool a clone holds), by name.
 *   Two units that ship copies of one name install the one the session read (its first read of
 *   that name, placed by attribute.ts's rules: the host gave the session that copy) when a unit
 *   ships it then, else the first unit's. So every skill of a profile is in its repository at the
 *   commit the profile names; the evidence names each skill installers.tsv lists that the unit did
 *   not ship then, and each it shipped then that installers.tsv does not list. Plugins: an Oh My
 *   Pi package's directory (the `omp-package` manifest's, at that commit), or a Claude Code
 *   plugin's directory (its skills' parent), named by its last part; a package at its
 *   repository's root (a `github:` dependency's) by the name it is installed under.
 * - The session repository, the repository the session's working directory is a checkout of
 *   (`sessionRepository`), and the working directory's path inside that checkout: for a Legion
 *   context, the repository of its workspace (`/legion/workspaces/<owner>/<name>/<workspace>/…`);
 *   for a dev box, the repository of the copies output's list (repositories.tsv) named as the last
 *   part of its working directory, or of the nearest parent named as any listed repository, when
 *   one alone has that name and, for a parent, its tree holds the working directory's path below
 *   that parent (otherwise none, and the evidence says why or names the repositories that
 *   matched); the checkout's root is that directory. A working directory directly inside such a
 *   parent whose tree does not hold it is a workspace of that repository (a jj or git worktree)
 *   and the checkout's root, where a file the session read by an absolute path under it is a file
 *   of the tree at its path there (DEVBOX_CHECKOUT_RULE). The profile names it and its commit at
 *   the session's start (`sessionRepository`; load.ts places its `.claude/skills` project skills
 *   in Claude Code's checkout by it). Its project skills, read at its commit with
 *   the units' repositories (when it is no unit's repository, only where Oh My Pi reads a project,
 *   one directory a call, `GithubReads.project`, so a repository whose whole tree GitHub cuts
 *   short is read too): each `<level>/.omp/skills/<name>/SKILL.md` that has a description,
 *   then each `<level>/.claude/skills/<name>/SKILL.md`, the levels the working directory and each
 *   parent up to the checkout's root, the nearest first (Oh My Pi 18.18.6's discovery/builtin.ts,
 *   lines 81-109 and 314-327, and discovery/claude.ts, lines 205-224). A project skill and another
 *   skill of one name: the one the session read, else the one Oh My Pi admits first
 *   (PROJECT_PRECEDENCE: the project's `.omp/skills` before `~/.omp/agent/skills` and extension
 *   packages, its `.claude/skills` after those and after `~/.claude/skills` and before Claude Code
 *   plugins, the nearer level of one kind first, and a pool an extension's `resources_discover`
 *   names before them all); the other is listed as left out.
 * - Context files: the session repository's, one at each level from the working directory up to
 *   the checkout's root, the one Oh My Pi 18.18.6 keeps in that level's slot (CONTEXT_FILES, in
 *   its providers' order: `.omp/AGENTS.md` at the nearest level whose `.omp/` holds anything,
 *   `.claude/CLAUDE.md` at the working directory, `.agent/AGENTS.md` and `.agents/AGENTS.md` at
 *   each level, `.gemini/GEMINI.md` and `.github/copilot-instructions.md` at the working
 *   directory, then `AGENTS.md` and `CLAUDE.md` at each level), listed the farthest level first,
 *   each at its path in the repository; the profile's `workingDirectory` names the working
 *   directory's path in the checkout (context.ts writes the files in its checkout and runs the
 *   capture there). For a Legion context, the project's instructions file too, named by its
 *   project file (a `legion-project` manifest of manifests.tsv, `instructions:` read at that
 *   commit, under `<dir>/instructions/`). A devbox session has the user-level context file Oh My
 *   Pi 18.18.6 reads (discovery/claude.ts), marked `user: true`: one file at user level,
 *   CLAUDE.md in Claude Code's config directory, which a dev box's CLAUDE_CONFIG_DIR names, the
 *   `.claude/` of the repository that installs its units. That is the one repository of the
 *   profile's units that holds Claude Code's settings there (`.claude/settings.json`, copies.ts's
 *   claude-settings manifest) at the session's start, and its `.claude/CLAUDE.md` at that commit
 *   (context.ts writes it as `CLAUDE.md` in a scratch Claude Code config directory that the
 *   capture names in CLAUDE_CONFIG_DIR, where Oh My Pi reads it as its user-level file; load.ts
 *   gives Claude Code it as user memory). Each one the copies output or the session does not name
 *   is listed as left out: for a devbox, when no repository or more than one of its units holds
 *   those settings, or that one has no CLAUDE.md then. A subagent's case has none: its recorded
 *   prompt holds them. The transcript's config root decides which units and skills to read.
 *
 * Output: the case file `--out` (one JSON line per case, written whole after each case and checked
 * with cases.ts's `loadCases` against `--old` at the end) and `<out>.summary.json`: the seed and
 * the order, the sessions tried, each session's outcome, model and the reader's dollars on it, the
 * cases built, the drops by reason, the sessions tried and the drops of each model (`byModel`), the
 * bounds, the pulls, the gateway's spend, the GitHub reads sent again after a transient failure
 * by reason (`githubRetries`, summed over the runs of one `--out`), and why the build stopped, if
 * it did.
 *
 * **build-cases --reprofile** builds the profile of each case of a case file again, from the
 * case and its session's head: its store, the `[Skill file: …]` lines of its messages, the
 * paths of its whole reads by path of its `pathCopy` and of its reads by an absolute path, its
 * start, whether it carries a recorded prompt, and the working directory the header in the
 * session's head
 * names (capture.ts's `sessionHeads`: session-head.sql through the store command, one query per
 * store, each head saved under `<out>.sessions/`), at the time the profile is read: its root's
 * prompt builds (digest-times.sql, one query per store), with a top-level case's step's message
 * time and digest (TOP_BUILD_RULE). It pulls no transcript and calls no reader. For
 * a case whose session the store no longer holds, the evidence says so (`no session head`), and
 * the session repository is the one the old case's evidence names (`session repository <repo>`,
 * or `no session repository`); a case built before that evidence was written is taken to be in
 * the workspace of the repository whose `AGENTS.md` the old profile holds, and one whose old
 * profile holds none keeps its old evidence of why.
 * The new file's cases differ from the old ones only in `profile` (its skills, plugins and context
 * files; a recorded prompt and its tools stay) and in the profile's part of `evidence`.
 * `<out>.summary.json` lists, per case, the skills added and removed, and the GitHub reads sent
 * again after a transient failure by reason (`githubRetries`). A case whose profile cannot be
 * built, or no longer installs its skill, is left out and named, and the command exits 1.
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
 * query's row count (or its error), the messages offered, each task's reason, and the spend. Its
 * queries, one per store and day, take most of its time (an hour and more on the real stores), so
 * it goes on as build-cases does: the summary records the inputs (SKILL.md's SHA-256, the stores
 * and the window), the terms and each query as it ends, and `<out>.found.jsonl` the messages each
 * found (path hashes and texts, appended whole before the summary records the query). A run of the
 * same inputs goes on from them with no terms call and no query already recorded; a finished
 * build is left as it is; other inputs are refused. The found file is removed once the tasks are
 * written.
 *
 * **build-tasks --first-read** (the load test's positive control) writes the control's need
 * tasks from the sessions file of the skill, unchosen: see `buildControlTasks`.
 *
 * **--profiles-out** (both modes): each task runs in the rebuilt context of the session it came
 * from. For each task from a session, the session is pulled again and its profile rebuilt as
 * build-cases rebuilds a case's (`sessionProfile`: the context, `caseProfile` from every read of
 * its branch with `--copies` and `--count`, a subagent's recorded system prompt and tools), a
 * top-level session's at its first build (the session's start: a task runs as its first message),
 * its measured subagent read from the store first (one subagents.sql query a session), since that
 * subagent's listing decides its pools; less the tested skill, and written to `<dir>/<task
 * id>.json` (load.ts's LoadProfile); the task line's `profile` is its path. A written task takes
 * the profile of `--context-session
 * <store>:<hash>` (required in the reader's mode; rebuilt before any reader call, and a session
 * that gives none fails the build), read at the build its listing measure's subagent was spawned
 * under (TOP_BUILD_RULE: that subagent, the earliest whose recorded prompt holds a listing, found
 * first with subagents.sql). Each profile keeps the listing its listing measure sets it against
 * (`withRecordedListing`; load.ts's RecordedListing): the session's own recorded prompt's, else
 * that subagent's, with where it was recorded, each listed skill's line, and the profile's copy
 * of the tested skill's source; the prompt is already read, so this needs no store read. A load
 * lists each skill under its recorded line where that line stands for the skill's text (load.ts).
 * The summary's `profiles` records the context session, each task's profile and its evidence, why
 * a task has none, and the GitHub reads sent again after a transient failure by reason
 * (`githubRetries`). A finished build gets no profiles from `--profiles-out` afterwards;
 * `--reprofile` (below) builds them again from its tasks file.
 * A task whose context does not list the tested skill tests nothing (`listsSkill`: a subagent's
 * recorded listing lacks its line, or another context's profile does not install it), so with
 * `--profiles-out` such a session is dropped (`skill-not-listed`) and counted: in the reader's
 * mode each offered message's session is rebuilt first and the next candidate taken in its
 * place (`offeredMessages`, `candidateDrops`), in `--first-read` when the session is tried
 * (`skillNotListed`); and a `--context-session` whose context does not list it fails the build
 * (exit 1, found after its store read).
 *
 * **build-tasks --reprofile <tasks.jsonl> --profiles-out <new dir>** builds the profile of each task
 * of a tasks file again as `--profiles-out` builds one now, with no candidate search and no reader
 * call: a task from a session takes its session's, found in its store by its path hash
 * (session-paths.sql, one query a store) and pulled as the search pulls it; a written task takes
 * that of `--context-session`, else of the context session the tasks file's summary
 * (`<tasks.jsonl>.summary.json`) names. It writes each profile to `<new dir>/<task id>.json`, the
 * tasks again to `<new dir>/tasks.jsonl` with each `profile` naming its new file, and
 * `<new dir>/tasks.jsonl.summary.json`: per task, whether the profile's bytes changed
 * (`unchanged`, `changed`) and the evidence items added and removed against the old summary's,
 * and in its `profiles` the GitHub reads sent again after a transient failure (`githubRetries`).
 * A task whose session its store holds no longer keeps its old profile (`kept`, with the evidence
 * item `no session in the store`). A session task whose rebuilt context no longer lists the skill
 * is left out and named, and the command exits 1; a store read, a pull or a rebuild that fails
 * fails it, with nothing written to `<new dir>`.
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
import { createHash, randomUUID } from "node:crypto";
import {
  appendFileSync,
  closeSync,
  existsSync,
  linkSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
  writeSync,
} from "node:fs";
import path from "node:path";
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { mapLimit, outRefusal } from "../skill-scenarios/production-dispatch";
import { processSpendRecorder, type SpendRecorder } from "../skill-scenarios/spend-record";
import {
  readCalls,
  readTargets,
  SessionEntry,
  SessionMessage,
  skillFileHeader,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import { HASHLINE_HEADER, pathFormFault } from "./arms";
import {
  CLAUDE_SETTINGS,
  devboxRepositories,
  type InstallersRow,
  type ManifestsRow,
  MEMBER_ACTIONS,
  modelId,
  type Placement,
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
import { armPromptBlocks, listingOf, type SavedHead, sessionHeads } from "./capture";
import {
  Case,
  listedName,
  loadCases,
  Profile,
  type ProfileSkill,
  pathHash,
  pathReadFile,
  READ_SELECTOR,
  resolvedReadPath,
  skillReadFile,
  Thinking,
  testedCopy,
  testedSource,
  textLines,
  URL_TARGET,
} from "./cases";
import {
  configFrom,
  fetchFailure,
  firstLine,
  GhRetries,
  httpFailure,
  installationRateWait,
  json,
  lastTryError,
  ompSkillName,
  ompText,
  parse,
  parseJson,
  RATE_LIMITED,
  Refused,
  readFrontmatter,
  required,
  type Sent,
  STORE_QUIET_MS,
  schemaFaults,
  transientReason,
  writeWhole,
} from "./common";
import { type DeploymentImage, OMP_VERSION, type SkillCutsConfig, storeNamed } from "./config";
import {
  composedAt,
  DAEMON_IMAGE_ITEM,
  linkTargetPath,
  NO_SESSION_REPOSITORY_ITEM,
  PROMPTS_GO,
  READ_TIMES_ITEM,
  SESSION_REPOSITORY_ITEM,
  skillTextFrontmatter,
} from "./context";
import {
  BLOB_BATCH,
  BLOB_BATCH_BYTES,
  BlobBatch,
  blobBatches,
  blobQuery,
  CopyIndex,
  dirOf,
  factsOf,
  type Gh,
  type GhResult,
  GitBlob,
  graphqlArgs,
  type InstallerPin,
  ignoredSkillMatch,
  installerPins,
  keepsEntry,
  type Manifest,
  npmPins,
  parseManifest,
  prefixMatches,
  releaseTag,
  repoPath,
  repositoryFiles,
  resolveInstallers,
  type SkillCopy,
  skillFields,
  spawnGh,
  type TreeEntry,
  tarballSkills,
} from "./copies";
import {
  digestTimesSql,
  mtimeRange,
  PULL_PAGE,
  pullSql,
  sessionPathsSql,
  sessionsSql,
  subagentsSql,
  tasksSql,
} from "./countsql";
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
import { EvidenceItem, messageLoad, type RecordedListing, Task } from "./load";
import {
  builtOn,
  finalStage,
  type ImageConfig,
  type PodImage,
  PodNotMeasurable,
  type ReadImageConfig,
  registryImageConfig,
  sessionPodImage,
} from "./pod-image";
import { eachStoreRow, PullCheck, storeRows } from "./psql-rows";
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
import { LISTING_MEASURE_ITEM } from "./score";
import { sessionProfileName } from "./session-root";

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
  "no-read":
    "no read of a file of the skill succeeded, by a skill:// URL or by its path (count.sql's rule)",
  "path-range":
    "the session's only successful reads of the skill are reads by path that no case is built from, and one has a read selector (a line range, `:raw`), which the arms do not answer",
  "other-copy":
    "the session's only successful reads of the skill are reads by path that no case is built from, none with a selector, and one reads another copy: attribute.ts's rules place it at another repository's copy, or it is a URL",
  unattributed:
    "the session's only successful reads of the skill are reads by path that no case is built from, none with a selector or of another copy, and one is at a path attribute.ts's rules place at no copy",
  "path-relative":
    "the session's only successful reads of the skill are whole reads by path of the cut's copy whose relative or `~` path needs a directory the session does not name (its header names no working directory, or its transcript's path no home, `<home>/.omp/…`), or that resolves to no absolute directory, so a case cannot name the copy (cases.ts's pathCopy)",
  "path-form":
    "a file of the cut's copy that the case's messages read whole by path is one the arms do not answer in Oh My Pi's form (arms.ts's pathFormFault, or a directory), in the old text or, when --new is given, the new",
  "not-cut-copy":
    "attribute.ts's rules (the prefix table, a checkout, the store's context) do not place the skill's first read at the cut repository's copy, or the profile installs no copy of it",
  "no-role":
    "a Legion session's role is in none of the forms the builder reads (module comment, The role)",
  "bad-session-init":
    "a subagent's session_init line holds no system prompt, no tool names or no agent name",
  "reader-failed": "the case reader's every reply was refused",
  "reader-error": "the case reader's call failed",
  "no-step": "the case reader found no step the skill governs",
  "skill-as-material":
    "the case reader finds that the agent's work at the skill's read is on the skill's own text (editing, rewriting, reviewing, testing, grading, scoring or comparing the skill or a copy of it), so its next step works on that text and does not follow it",
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
  "read-by-path":
    "a read before the step reads a file of the skill in a way the case neither holds nor names: a skill:// file with a selector, or a path that ends `/<skill>/SKILL.md`, holds `skills/<skill>/` or is under the skill's installed directory and is neither a whole read of the case's copy (pathCopy) nor an excluded read of its evidence (an error result of such a read too)",
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
/** The user memory file beside Claude Code's settings (CLAUDE_SETTINGS, as copies.ts reads them
 *  from its claude-settings manifest), which Oh My Pi reads as its user-level context file
 *  (discovery/claude.ts at the pinned tag). */
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
  /** Waits `ms` before a GitHub or registry read that failed for a transient reason is sent again
   *  (`Bun.sleep` when absent). */
  sleep?(ms: number): Promise<void>;
  /** One try of a read of the registry's tarball of a package version (`registryTarball` when
   *  absent); `GithubReads.tarball` sends a try that failed for a transient reason again. */
  readonly npm?: NpmTarball;
  /** Reads an image's config from its registry (`registryImageConfig` with the configuration's
   *  `registries`, each request sent again by the GitHub reads' rule, when absent). */
  readonly imageConfig?: ReadImageConfig;
  /** Where each reader's answer records its usage span (spend-record.ts; `processSpendRecorder()`
   *  in a real run); none when absent. `main` leaves the wait for its spans to its caller. */
  readonly spendRecorder?: SpendRecorder;
}

/** The file in a spool directory (`spoolDir`) that says which run holds it: `<pid> <token>`. */
const SPOOL_LOCK = "run.lock";
/** The spool locks each `main` call holds (by its io): each lock file and its text, which `main`
 *  gives up as the call ends. */
const spoolLocks = new WeakMap<BuildIo, { readonly lock: string; readonly text: string }[]>();

/** Whether the process `pid` runs: a signal 0 reaches it, or the system refuses to send one. */
function processLives(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return hasErrnoCode(error, "EPERM");
  }
}

/**
 * The directory a build pulls transcripts into, `<out>.spool`, held by this run and made empty: one
 * file for each session open, removed once the session is decided, and the directory once the
 * build ends. The run holds it by its lock file (SPOOL_LOCK, holding its pid), so no two runs of
 * one `--out` share a spool, and none empties or removes the files of another: while the lock's
 * process runs, the directory is refused (exit 2), naming it. The lock is made in one step with
 * its pid: written whole to a file of this run's, which is then linked as SPOOL_LOCK, a link that
 * fails while a lock is there; so no run reads a lock whose pid is not written yet. A lock that
 * is gone when it is read (its run gave it up in between) is tried again. A lock whose process
 * runs no more (a build that was killed) is taken over, and `io.err` says so: it is first moved
 * to a name of this run's (one rename, so of two runs that found it dead, one moves it), and a
 * run that moved a lock whose text is not the dead one's (another run's lock, made since it was
 * read) puts that lock back and reads it again. `main` gives the lock up as the command ends,
 * with the directory when nothing else is left in it.
 */
function spoolDir(out: string, io: BuildIo): string {
  const dir = `${out}.spool`;
  const lock = path.join(dir, SPOOL_LOCK);
  const token = randomUUID();
  const text = `${process.pid} ${token}`;
  const made = path.join(dir, `${SPOOL_LOCK}.${token}`);
  for (let tries = 0; ; tries += 1) {
    try {
      mkdirSync(dir, { recursive: true });
      writeFileSync(made, `${text}\n`);
      linkSync(made, lock);
      break;
    } catch (error) {
      // EEXIST: a lock is there. ENOENT: the run that holds it emptied the spool, this run's file
      // with it, or gave it up with its directory, between this run's steps.
      const locked = hasErrnoCode(error, "EEXIST") || hasErrnoCode(error, "ENOENT");
      if (!locked || tries > 2) throw error;
    } finally {
      rmSync(made, { force: true });
    }
    let held: string;
    try {
      held = readFileSync(lock, "utf8").trim();
    } catch (error) {
      if (hasErrnoCode(error, "ENOENT")) continue;
      throw error;
    }
    const pid = Number(held.split(" ")[0]);
    if (Number.isInteger(pid) && pid > 0 && processLives(pid)) {
      throw new Refused(
        `${dir} is in use: the run of process ${pid} holds its lock ${lock} ("${held}"), so this run would empty the spool under it; wait for that run to end, or give another --out`
      );
    }
    const aside = path.join(dir, `${SPOOL_LOCK}.${token}.dead`);
    try {
      renameSync(lock, aside);
    } catch (error) {
      if (hasErrnoCode(error, "ENOENT")) continue;
      throw error;
    }
    const moved = readFileSync(aside, "utf8").trim();
    if (moved !== held) {
      try {
        linkSync(aside, lock);
      } catch (error) {
        if (!hasErrnoCode(error, "EEXIST")) throw error;
        throw new Refused(
          `${dir} is in use: this run took the lock ${lock} ("${moved}") of another run for the dead one it read ("${held}"), and a third run made its own lock before this run could put it back; stop the runs of this --out and start one`
        );
      } finally {
        rmSync(aside, { force: true });
      }
      continue;
    }
    rmSync(aside, { force: true });
    io.err(
      `the lock ${lock} ("${held}") is of process ${Number.isInteger(pid) ? pid : "unknown"}, which runs no more: this run takes it over`
    );
  }
  for (const entry of readdirSync(dir)) {
    if (entry !== SPOOL_LOCK) rmSync(path.join(dir, entry), { recursive: true, force: true });
  }
  spoolLocks.set(io, [...(spoolLocks.get(io) ?? []), { lock, text }]);
  return dir;
}

/** Gives up each spool lock the `main` call of `io` holds (`spoolDir`): its lock file, when it is
 *  still this run's, and its directory, when nothing else is left in it. */
function releaseSpools(io: BuildIo): void {
  for (const { lock, text } of spoolLocks.get(io) ?? []) {
    if (existsSync(lock) && readFileSync(lock, "utf8").trim() === text)
      rmSync(lock, { force: true });
    const dir = path.dirname(lock);
    if (existsSync(dir) && readdirSync(dir).length === 0)
      rmSync(dir, { recursive: true, force: true });
  }
  spoolLocks.delete(io);
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

/** Pulls the file `filePath` of the store `db` into the file `spool`, PULL_PAGE rows a call, each
 *  row checked by psql-rows.ts's `PullCheck` and its bytes written as it comes (`eachStoreRow`),
 *  so one row (a slice of 1 MiB and at most 3 bytes more), with the chunks of psql's output it
 *  spans, is held at a time however long the file; a call that prints nothing for `quietMs` is
 *  ended and fails the pull. Returns the bytes it wrote and the calls it took. */
export async function pullTranscript(
  command: string,
  db: string,
  filePath: string,
  spool: string,
  quietMs = STORE_QUIET_MS
): Promise<{ bytes: number; calls: number }> {
  const fd = openSync(spool, "w");
  try {
    const check = new PullCheck();
    const write = (row: unknown) => {
      const text = check.take(row);
      for (let at = 0; at < text.length; ) at += writeSync(fd, text, at);
    };
    for (let calls = 1; ; calls += 1) {
      const page = await eachStoreRow(command, db, pullSql(filePath, check.last), write, quietMs);
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

/** The paths of the files of `hashes` in the store `db`, by hash, in one query of `query` (none
 *  for no hash): sessions.sql (`sessionsSql`, file hashes) unless another is given, such as
 *  session-paths.sql (`sessionPathsSql`, path hashes). Either reads the store's whole file table
 *  once, whatever the hashes. */
async function resolvePaths(
  command: string,
  db: string,
  hashes: readonly string[],
  query: (hashes: readonly string[]) => string = sessionsSql
): Promise<{ paths: Map<string, string>; queries: number }> {
  const paths = new Map<string, string>();
  if (hashes.length === 0) return { paths, queries: 0 };
  for (const row of await storeRows(command, db, query(hashes))) {
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

/** A read of a file of the skill in the messages: the read result's index, its call's index, the
 *  file it read (a path inside the skill's directory), the path its `[Skill file: …]` line names,
 *  and whether it succeeded. `path`, for a whole read by path of the cut's copy (`copyReads`):
 *  the path it read, whose result shows Oh My Pi's rows and no `[Skill file: …]` line. */
interface SkillRead {
  readonly index: number;
  readonly call: number;
  readonly file: string;
  readonly header: string | undefined;
  readonly ok: boolean;
  readonly path?: string;
}

/** The index in `messages` of each tool call's message, by the call's id. */
function callIndices(messages: readonly SessionMessage[]): Map<string, number> {
  const calls = new Map<string, number>();
  for (const [index, message] of messages.entries()) {
    if (message.role !== "assistant") continue;
    for (const block of message.content ?? []) {
      if (block.type === "toolCall" && block.id !== undefined) calls.set(block.id, index);
    }
  }
  return calls;
}

/** Each read result of a `skill://<skill>` file in `messages`, in order. */
function skillReads(messages: readonly SessionMessage[], skill: string): SkillRead[] {
  const targets = readTargets(messages);
  const calls = callIndices(messages);
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

/** Whether a read of `target` reads a file of skill `skill` by its path, by count.sql's rule: a
 *  path or URL (no `skill://` one) that, its read selector taken off, ends `<skill>/SKILL.md` or
 *  holds `skills/<skill>/references/` and more, each after a `/` or at its start. */
function readsByPath(target: string, skill: string): boolean {
  if (target.startsWith("skill://")) return false;
  const file = `/${target.replace(READ_SELECTOR, "")}`;
  const references = file.indexOf(`/skills/${skill}/references/`);
  return (
    file.endsWith(`/${skill}/SKILL.md`) ||
    (references !== -1 && file.length > references + `/skills/${skill}/references/`.length)
  );
}

/** The kinds of a read by path of the skill that no case is built from, in the order a session
 *  whose only successful reads of the skill are such reads takes its drop reason from: a read
 *  selector, another copy (or a URL), a path no rule places, the cut's copy at a path the build
 *  cannot resolve to an absolute directory (`pathKind`). */
const EXCLUDED_KINDS = [
  "path-range",
  "other-copy",
  "unattributed",
  "path-relative",
] as const satisfies readonly DropReason[];
type ExcludedKind = (typeof EXCLUDED_KINDS)[number];

/** The check of a case's `pathCopy.dir` (cases.ts): an absolute directory with no empty, `.` or
 *  `..` part and no `/` at its end. Its `cwd` and `home` are held to it too. */
const PATH_COPY_DIR = Case.shape.pathCopy.unwrap().shape.dir;

/** A case's copy of its skill as its session read it by path (cases.ts's `pathCopy`): its
 *  directory, and the working and home directories a read's relative or `~` path resolves
 *  against (cases.ts's `resolvedReadPath`). */
type CopyPlace = NonNullable<Case["pathCopy"]>;

/** The directories a session's reads by path resolve against (cases.ts's `resolvedReadPath`):
 *  `cwd`, the working directory its header names; `home`, the home directory it ran under, the
 *  part of its transcript's path before the first `/.omp/`. Oh My Pi resolves `~` against the
 *  home directory (packages/utils/src/path.ts lines 59-70 at the pin) and writes its sessions
 *  under its config root in that same home, `<home>/.omp/agent/sessions/` or a profile's
 *  `<home>/.omp/profiles/<name>/agent/sessions/` (packages/utils/src/dirs.ts lines 127-146 and
 *  360-363), so the transcript's path names it; a session whose config root another variable
 *  moved names none. Each is left out when it is no absolute directory. */
function sessionDirs(cwd: string | undefined, transcriptPath: string): Omit<CopyPlace, "dir"> {
  const absolute = (dir: string | undefined) => {
    const resolved =
      dir !== undefined && path.posix.isAbsolute(dir) ? path.posix.resolve(dir) : undefined;
    return resolved !== undefined && PATH_COPY_DIR.safeParse(resolved).success
      ? resolved
      : undefined;
  };
  const at = transcriptPath.indexOf("/.omp/");
  const work = absolute(cwd);
  const home = absolute(at > 0 ? transcriptPath.slice(0, at) : undefined);
  return { ...(work === undefined ? {} : { cwd: work }), ...(home === undefined ? {} : { home }) };
}

/** A read by path of a file of the skill in the messages (`readsByPath`): the read result's index,
 *  its call's index, the path read, whether it succeeded, and its kind. A `source` is a whole read
 *  that attribute.ts's rules place at the cut repository's copy, at the absolute directory `dir`
 *  (the path up to the skill's directory, attribute.ts's `copyPrefix`, resolved as Oh My Pi's read
 *  resolves it: cases.ts's `resolvedReadPath`): a case can be built from it. Any other kind keeps
 *  it out of a case (EXCLUDED_KINDS). `why` says why it is of its kind. */
interface PathRead {
  readonly index: number;
  readonly call: number;
  readonly target: string;
  readonly ok: boolean;
  readonly kind: "source" | ExcludedKind;
  readonly dir?: string;
  readonly why: string;
}

/** The kind of a read by path of `target` (PathRead), placed by `place` (attribute.ts's rules, on
 *  the path as read, as the count places it), held to the cut's repository `cutRepo`, and its
 *  directory resolved against the session's directories `dirs` (`sessionDirs`). A read of the cut's
 *  copy whose relative or `~` path needs a directory `dirs` lacks, or that resolves to no absolute
 *  directory a case can name, is `path-relative`. */
function pathKind(
  target: string,
  place: (copyPath: string) => Placement,
  cutRepo: string,
  dirs: Omit<CopyPlace, "dir">
): Pick<PathRead, "kind" | "dir" | "why"> {
  if (READ_SELECTOR.test(target)) {
    return { kind: "path-range", why: "a read selector after its last /" };
  }
  if (URL_TARGET.test(target)) {
    return { kind: "other-copy", why: "a URL, read from the web at the commit it names" };
  }
  const placed = place(target);
  if (placed.copy === undefined) {
    const why = placed.outcome?.replace(/^unattributed: /, "") ?? "no rule places it at a copy";
    return { kind: "unattributed", why };
  }
  const [repository = "", dir = ""] = placed.copy.split("\t");
  const where = `the ${placed.rule} rule places it at ${repository} ${dir}`;
  if (repository !== cutRepo) return { kind: "other-copy", why: where };
  const copyDir = placed.prefix.replace(/\/$/, "");
  const resolved = resolvedReadPath(copyDir, dirs);
  if (resolved === undefined) {
    // A path that does not resolve with a working directory given needs a home (cases.ts's
    // resolvedReadPath: `~`, `~/…`, `~name/…`, `@~`, `@~/…`); any other needs the working
    // directory (`@~name/…` too, which Oh My Pi's read takes relative to it).
    const needs =
      resolvedReadPath(copyDir, { cwd: "/" }) === undefined
        ? "the home directory the session ran under, which its transcript's path does not name (no /.omp/ in it)"
        : "the session's working directory, which its header does not name";
    return { kind: "path-relative", why: `${where}, at ${copyDir}, which needs ${needs}` };
  }
  if (!PATH_COPY_DIR.safeParse(resolved).success) {
    return {
      kind: "path-relative",
      why: `${where}, at ${resolved}, no absolute directory a case can name the copy by`,
    };
  }
  return { kind: "source", dir: resolved, why: where };
}

/** Each read result of `messages` whose call reads a file of skill `skill` by its path
 *  (`readsByPath`), in order, with its kind (`pathKind`). */
function pathReads(
  messages: readonly SessionMessage[],
  skill: string,
  place: (copyPath: string) => Placement,
  cutRepo: string,
  dirs: Omit<CopyPlace, "dir">
): PathRead[] {
  const targets = readTargets(messages);
  const calls = callIndices(messages);
  const reads: PathRead[] = [];
  for (const [index, message] of messages.entries()) {
    if (message.role !== "toolResult" || message.toolName !== "read") continue;
    const target = targets.get(message.toolCallId ?? "");
    if (target === undefined || !readsByPath(target, skill)) continue;
    reads.push({
      index,
      call: calls.get(message.toolCallId ?? "") ?? index,
      target,
      ok: message.isError !== true,
      ...pathKind(target, place, cutRepo, dirs),
    });
  }
  return reads;
}

/** A read by path no case is built from, as a drop's detail and a case's evidence name it. A
 *  `source` read is excluded only for being at another directory than the case's `pathCopy`. */
function excludedRead(read: PathRead, pathCopy?: { readonly dir: string }): string {
  const kind =
    read.kind === "source" ? `another directory than the case's copy ${pathCopy?.dir}` : read.kind;
  return `message ${read.index} answers the read of ${read.target} (${kind}: ${read.why})`;
}

/** Each read result of `messages` whose call reads a file of the cut's copy `pathCopy` whole by
 *  its path (cases.ts's `pathReadFile`, which resolves a relative or `~` path with the copy's
 *  `cwd` and `home`), in order, as a SkillRead of that file. */
function copyReads(messages: readonly SessionMessage[], pathCopy: CopyPlace): SkillRead[] {
  const targets = readTargets(messages);
  const calls = callIndices(messages);
  return messages.flatMap((message, index): SkillRead[] => {
    if (message.role !== "toolResult" || message.toolName !== "read") return [];
    const target = targets.get(message.toolCallId ?? "");
    const file = target === undefined ? undefined : pathReadFile({ pathCopy }, target);
    if (target === undefined || file === undefined) return [];
    return [
      {
        index,
        call: calls.get(message.toolCallId ?? "") ?? index,
        file,
        header: undefined,
        ok: message.isError !== true,
        path: target,
      },
    ];
  });
}

/** Each whole read by path of a file of the copies `copies` in `messages` (`copyReads`), once,
 *  in order. */
function copiesReads(
  messages: readonly SessionMessage[],
  copies: readonly CopyPlace[]
): SkillRead[] {
  const reads = new Map<number, SkillRead>();
  for (const copy of copies) {
    for (const read of copyReads(messages, copy)) {
      if (!reads.has(read.index)) reads.set(read.index, read);
    }
  }
  return [...reads.values()].sort((a, b) => a.index - b.index);
}

/** The text of the file a successful read by path shows: the result's text less what Oh My Pi
 *  puts around the lines (arms.ts's `PathRows`), the `[<path>#<tag>]` line and each row's `<n>:` of
 *  hashline rows, or each row's `<n>|` of numbered rows (`details.rowPrefixes`); plain rows as
 *  they are. */
function pathReadText(message: SessionMessage): string {
  const text = message.content?.[0]?.text ?? "";
  const lines = text.split("\n");
  if (HASHLINE_HEADER.test(lines[0] ?? "")) {
    return lines
      .slice(1)
      .map((line) => line.replace(/^\d+:/, ""))
      .join("\n");
  }
  const details = message.details;
  if (isObject(details) && Reflect.get(details, "rowPrefixes") === true) {
    return lines.map((line) => line.replace(/^\d+\|/, "")).join("\n");
  }
  return text;
}

/** The text of the file the read `read` read, from its whole result `message`: a `skill://` read's
 *  less its `[Skill file: …]` line, a read by path's less Oh My Pi's rows (`pathReadText`). */
function readText(read: SkillRead, message: SessionMessage | undefined): string {
  if (message === undefined) return "";
  return read.path === undefined
    ? stripSkillHeader(message.content?.[0]?.text ?? "")
    : pathReadText(message);
}

/** Why the arms do not answer a whole read by path of one of `files` (paths inside the cut's copy)
 *  in Oh My Pi's form from the old text or the new (arms.ts's `pathFormFault`), or undefined: the
 *  arm whose text it is, its directory and the fault. With no new text (a build-cases run given no
 *  `--new`), the old text alone. A file a text lacks they answer as not found; one a text holds as
 *  a directory they do not answer. cli.ts's behaviour screen drops a case on it too. */
export function pathFormIn(
  files: readonly string[],
  texts: { readonly old: string; readonly new?: string | undefined }
): { readonly arm: "old" | "new"; readonly dir: string; readonly fault: string } | undefined {
  const arms: (readonly ["old" | "new", string])[] = [
    ["old", texts.old],
    ...(texts.new === undefined ? [] : [["new", texts.new] as const]),
  ];
  for (const file of new Set(files)) {
    for (const [arm, dir] of arms) {
      const full = path.join(dir, file);
      const stats = statSync(full, { throwIfNoEntry: false });
      if (stats === undefined) continue;
      const fault = stats.isFile()
        ? pathFormFault(file, readFileSync(full))
        : `${file} is no file, and the arms answer reads of files only`;
      if (fault !== undefined) return { arm, dir, fault };
    }
  }
  return undefined;
}

/** A successful read result whose first line is `[Skill file: <path>]`: its index in the
 *  messages, that path, and the skill its call names (`skill://<skill>`), when it names one. With
 *  `byPath`, a whole read by path of the cut's copy (`copyReads`): `header` is the path it read,
 *  and `skill` the case's skill. */
interface ShownRead {
  readonly index: number;
  readonly header: string;
  readonly skill: string | undefined;
  readonly byPath?: true;
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

/** The reads a profile is built from: each of `shownReads`, and each successful whole read by path
 *  of a file of the cut's copies `copies` of `skill` (`copyReads`), in order. */
function profileReads(
  messages: readonly SessionMessage[],
  skill: string,
  copies: readonly CopyPlace[]
): ShownRead[] {
  const byPath = copiesReads(messages, copies).flatMap((read): ShownRead[] =>
    read.ok && read.path !== undefined
      ? [{ index: read.index, header: read.path, skill, byPath: true }]
      : []
  );
  return [...shownReads(messages), ...byPath].sort((a, b) => a.index - b.index);
}

/** A successful read result of a read call that names an absolute path: its index in the
 *  messages, and the path Oh My Pi's read opened (cases.ts's `resolvedReadPath`, its read selector
 *  taken off), normalized. */
interface AbsoluteRead {
  readonly index: number;
  readonly path: string;
}

/** Each successful read result of `messages` whose call names an absolute path, any file's, in
 *  order. */
function absoluteReads(messages: readonly SessionMessage[]): AbsoluteRead[] {
  const targets = readTargets(messages);
  return messages.flatMap((message, index) => {
    if (message.role !== "toolResult" || message.toolName !== "read" || message.isError === true) {
      return [];
    }
    const target = targets.get(message.toolCallId ?? "");
    const read =
      target === undefined ? undefined : resolvedReadPath(target.replace(READ_SELECTOR, ""), {});
    return read === undefined ? [] : [{ index, path: path.posix.normalize(read) }];
  });
}

/** The lines of each base file in `files` that a successful read of it in `reads` holds whole
 *  (`readText`: its text after the `[Skill file: …]` line, or less Oh My Pi's rows for a read by
 *  path; each line trimmed, as the builder's check of a proposed line compares them; blank lines
 *  aside), each with its number in the base file. A file no read holds a line of is not offered.
 *  `message` gives a read's whole message by its index. */
function offeredFiles(
  files: readonly { readonly path: string; readonly text: string }[],
  reads: readonly SkillRead[],
  message: (index: number) => SessionMessage
): CaseInput["files"] {
  const texts = new Map(
    reads
      .filter((read) => read.ok)
      .map((read): [number, string] => [read.index, readText(read, message(read.index))])
  );
  return files.flatMap((file) => {
    const held = new Set(
      reads
        .filter((read) => read.ok && read.file === file.path)
        .flatMap((read) => textLines(texts.get(read.index) ?? ""))
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
 *  ends `/<skill>/SKILL.md`, holds `skills/<skill>/`, or is under one of `installedDirs`. A call
 *  whose id `kept` holds (one the case holds as a read of its copy, or names as an excluded read)
 *  is passed over. */
function readByPath(
  messages: readonly SessionMessage[],
  skill: string,
  installedDirs: readonly string[],
  kept: ReadonlySet<string>
): string | undefined {
  for (const [index, message] of messages.entries()) {
    for (const { id, target } of readCalls([{ message }])) {
      if (kept.has(id)) continue;
      if (readsSkillUrl(target, skill)) {
        if (skillReadFile(skill, target) === undefined) {
          return `message ${index} reads ${target}, with a selector`;
        }
        continue;
      }
      if (URL_TARGET.test(target)) continue;
      const file = target.replace(READ_SELECTOR, "");
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

/** The digest a `system-prompt-digest` entry holds, or undefined for any other entry. */
function promptDigest(entry: SessionEntry): string | undefined {
  return entry.type === "custom" &&
    entry.customType === "system-prompt-digest" &&
    typeof entry.data === "string"
    ? entry.data
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
/** GitHub's attestations of an artifact's digest: each one's Sigstore bundle, whose DSSE
 *  envelope's payload is an in-toto statement in base64. */
const Attestations = z.looseObject({
  attestations: z.array(
    z.looseObject({
      bundle: z
        .looseObject({ dsseEnvelope: z.looseObject({ payload: z.string() }).optional() })
        .nullable()
        .optional(),
    })
  ),
});
/** An in-toto statement of SLSA build provenance v1, as far as its resolved dependencies. */
const Provenance = z.looseObject({
  predicate: z.looseObject({
    buildDefinition: z.looseObject({
      resolvedDependencies: z.array(
        z.looseObject({
          uri: z.string().optional(),
          digest: z.looseObject({ gitCommit: z.string().optional() }).optional(),
        })
      ),
    }),
  }),
});

/** What a repository holds at a commit, as copies.ts reads one: its skill copies, its manifest
 *  files parsed, and each file of those that could not be read or parsed, with why; for the
 *  context files a session in a checkout of it gets (`CONTEXT_FILES`), the size of each file of
 *  those names, by path, and each directory whose `.omp/` holds anything (`""` the root); and the
 *  directory of each copy Oh My Pi lists no line of (HIDDEN_RULE, read by the rule the capture's
 *  context uses: context.ts's `skillTextFrontmatter`). */
interface RepositoryAt {
  readonly copies: readonly SkillCopy[];
  readonly manifests: readonly Manifest[];
  readonly faults: readonly { readonly file: string; readonly why: string }[];
  readonly contextSizes: ReadonlyMap<string, number>;
  readonly ompLevels: ReadonlySet<string>;
  readonly hidden: ReadonlySet<string>;
}

/** A symbolic link a project skills directory holds (GithubReads' `project`): its path, its text,
 *  and whether it was followed to a directory of the repository, leaves the repository, or names
 *  no directory of it. */
interface ProjectLink {
  readonly link: string;
  readonly target: string;
  readonly outcome: "followed" | "outside" | "no directory";
}

/** What GithubReads' `project` gives: the repository as Oh My Pi reads a project in it, the tree
 *  reads it made, each skill link it met, and each of its levels the repository holds no
 *  directory at (`missing`). */
interface ProjectRead {
  readonly at: RepositoryAt;
  readonly reads: number;
  readonly links: readonly ProjectLink[];
  readonly missing: readonly string[];
}

/** Where Oh My Pi 18.18.6 says a skill it loads is left out of the system prompt's listing. */
const HIDDEN_RULE =
  "Oh My Pi 18.18.6 sets a skill's hide from its SKILL.md frontmatter's hide: true or disable-model-invocation: true (extensibility/skills.ts, lines 289, 494, 554 and 649; packages/utils/src/frontmatter.ts, lines 14-44, reads disable-model-invocation as disableModelInvocation), loads it, so skill:// reads it, and lists only the skills whose hide is not true (system-prompt.ts, lines 961-983)";

/** `cache`'s promise for `key`, made by `make` when it holds none. A promise that rejects with a
 *  gh call that failed for a transient reason (`GhFailed`: each read it made was sent again until
 *  no try was left) is dropped once it does, so a later caller reads again; any other rejection
 *  is kept. */
function readOnce<T>(
  cache: Map<string, Promise<T>>,
  key: string,
  make: () => Promise<T>
): Promise<T> {
  const held = cache.get(key);
  if (held !== undefined) return held;
  const made = make();
  cache.set(key, made);
  made.catch((error: unknown) => {
    if (transientFailure(error) && cache.get(key) === made) cache.delete(key);
  });
  return made;
}

/** A gh call that failed: the message names the call and gh's first error line; `transient` is
 *  the reason of common.ts's TRANSIENT_FAILURES that gh's error text names (`transientReason`),
 *  read from that text alone, so a repository or path that holds such a word is no transient
 *  failure. */
class GhFailed extends Error {
  constructor(
    message: string,
    readonly transient: string | undefined
  ) {
    super(message);
  }
}

/** Whether `error` is a gh call that failed for a transient reason (GhFailed). */
function transientFailure(error: unknown): boolean {
  return error instanceof GhFailed && error.transient !== undefined;
}

/** What `GithubReads.commitAt` throws when GitHub gives no commit of the ref at or before the
 *  time: the ref had no commit then. Callers tell it apart from every other failure by its type. */
class NoCommitThen extends Error {}

/** The reason a gh read that failed (`result`, as `GithubReads.send` gave it) failed for, when it
 *  failed for a transient reason on every try: the reason of common.ts's TRANSIENT_FAILURES that
 *  the last try's error names, or `rate limit` for a read GitHub still refused for its rate limit
 *  after every wait; undefined otherwise. Read from the last try alone (`lastTryError`): a read
 *  whose first try gave a 502 and whose last a 404 failed for good. */
function transientOf(result: Sent<GhResult>): string | undefined {
  const last = lastTryError(result);
  return transientReason(last) ?? (RATE_LIMITED.test(last) ? "rate limit" : undefined);
}

/** The standard output of `result`, gh's answer to `args`; throws a GhFailed naming `args` and
 *  gh's first error line (each try's, when the read was sent more than once) when it failed. A
 *  read GitHub still refused for its rate limit after every wait (`GithubReads.send`) is a
 *  transient failure (`transientOf`): its caller fails rather than taking the read as done (a
 *  skill left out of a profile because the limit was spent would change the case in silence). */
function ghAnswer(args: readonly string[], result: Sent<GhResult>): string {
  if (result.code !== 0) {
    const line = result.stderr.split("\n").find((each) => each.trim() !== "") ?? "";
    throw new GhFailed(`gh ${args.join(" ")} failed: ${line}`, transientOf(result));
  }
  return result.stdout;
}

/** One try of a read of the npm registry's tarball of `version` of the package `name`: its bytes,
 *  or undefined when the registry has none. A try that failed throws: an NpmFailed for a failure
 *  `GithubReads.tarball` may send again. */
export type NpmTarball = (name: string, version: string) => Promise<Uint8Array | undefined>;

/** The registry's tarball URL of `version` of the package `name` (`@scope/name` too). */
const tarballUrl = (name: string, version: string) =>
  `https://registry.npmjs.org/${name}/-/${name.split("/").pop()}-${version}.tgz`;

// A fetch has no time limit of its own: a try still running at this limit is given up and sent
// again.
const NPM_TIMEOUT_MS = 60_000;

/** A registry read that failed: the message names the URL and the failure; `failure` is the
 *  failure alone, in the words of common.ts's TRANSIENT_FAILURES when it is transient, so that it
 *  is read from those words alone (a package named connect-timeout makes no failure transient). */
class NpmFailed extends Error {
  constructor(
    url: string,
    readonly failure: string
  ) {
    super(`${url}: ${failure}`);
  }
}

/** The registry's tarball of a package version, one GET (`get`: fetch, or a test's stand-in) given
 *  up after `timeoutMs`, its body included: undefined for a 404. It throws an NpmFailed for an
 *  answer that is not OK, for a try past the limit and for a connection that failed, each in
 *  common.ts's words (`httpFailure`, `fetchFailure`), and any other failure as fetch threw it. A
 *  429, the registry's "too many requests", is sent again as a 5xx is. */
export async function registryTarball(
  name: string,
  version: string,
  timeoutMs = NPM_TIMEOUT_MS,
  get: (url: string, init: { readonly signal: AbortSignal }) => Promise<Response> = fetch
): Promise<Uint8Array | undefined> {
  const url = tarballUrl(name, version);
  const signal = AbortSignal.timeout(timeoutMs);
  try {
    const response = await get(url, { signal });
    if (response.status === 404) return undefined;
    if (!response.ok) throw new NpmFailed(url, httpFailure(response.status));
    return new Uint8Array(await response.arrayBuffer());
  } catch (error) {
    if (error instanceof NpmFailed) throw error;
    const failure = fetchFailure(error, signal, timeoutMs);
    if (failure === undefined) throw error;
    throw new NpmFailed(url, failure);
  }
}

/** The files of a gzipped tar (an npm tarball), by path less its first part (`package/`), each
 *  as Oh My Pi reads a file (common.ts's `ompText`): a regular file's entry only, its name the
 *  ustar prefix and name joined. */
function tarballFiles(gzipped: Uint8Array): Map<string, string> {
  const bytes = Bun.gunzipSync(new Uint8Array(gzipped));
  const text = (from: number, to: number) =>
    new TextDecoder().decode(bytes.subarray(from, to)).replace(/\0.*$/s, "");
  const files = new Map<string, string>();
  for (let at = 0; at + 512 <= bytes.length; ) {
    const name = text(at, at + 100);
    if (name === "") break;
    const size = Number.parseInt(text(at + 124, at + 136).trim() || "0", 8);
    const type = text(at + 156, at + 157);
    const prefix = text(at + 345, at + 500);
    const full = prefix === "" ? name : `${prefix}/${name}`;
    if (type === "" || type === "0") {
      const inside = full.split("/").slice(1).join("/");
      files.set(inside, ompText(bytes.subarray(at + 512, at + 512 + size)));
    }
    at += 512 + Math.ceil(size / 512) * 512;
  }
  return files;
}

/** The `from` of a default branch that the copies output's repositories.tsv names
 *  (`GithubReads.branch`). */
const LISTED_BRANCH = "repositories.tsv";

/** The read of the last commit of `ref` of `repo` at or before `at` (`GithubReads.commitAt`), as
 *  evidence names it. */
const commitsRead = (repo: string, ref: string, at: string) =>
  `gh api repos/${repo}/commits?sha=${ref}&until=${at}`;

/** The GitHub reads the profile makes, each made once, and each sent again after a transient
 *  failure as common.ts's `GhRetries` says (`retries`, which counts them): a read that failed so
 *  on every try is not kept, so a later caller reads it again. The registry's tarball read
 *  (`tarball`) is sent again by the same rule; the bytes are never kept, and the names of the
 *  skills a version's tarball ships are kept (`shippedSkills`). Exported for copies.test.ts,
 *  which checks that it reads a repository's files as copies.ts does. */
export class GithubReads {
  private readonly made = new Map<string, Promise<Sent<GhResult>>>();
  private readonly held = new Map<string, Promise<RepositoryAt>>();
  private readonly projects = new Map<string, Promise<ProjectRead>>();
  private readonly blobs = new Map<string, Promise<string>>();
  /** One level of a directory of a tree (`firstFile`), by repository, commit and path. */
  private readonly dirs = new Map<string, Promise<TreeEntry[]>>();
  /** The skills each package version's tarball ships (`shippedSkills`), by `<name>@<version>`. */
  private readonly shipped = new Map<string, Promise<ReadonlySet<string> | undefined>>();
  private readonly images = new Map<string, Promise<ImageConfig | undefined>>();

  constructor(
    private readonly gh: Gh,
    private readonly branches: ReadonlyMap<string, string>,
    readonly retries: GhRetries,
    /** One try of a read of the registry's tarball of a package version (`registryTarball` in a
     *  real run). */
    private readonly npm: NpmTarball = registryTarball,
    /** A read of an image's config from its registry (`registryImageConfig` of a public GHCR
     *  image alone, each request sent again as `retries` says, when absent). */
    private readonly readImageConfig: ReadImageConfig = (image) =>
      registryImageConfig(image, { retries })
  ) {}

  /** The OCI config of `image` from its registry (`readImageConfig`), kept for each image; a read
   *  that fails (a request that failed for a transient reason on every try, or a configured
   *  registry's refusal of its token) is not kept, so a later caller reads the registry again. */
  imageConfig(image: string): Promise<ImageConfig | undefined> {
    const held = this.images.get(image);
    if (held !== undefined) return held;
    const made = this.readImageConfig(image);
    this.images.set(image, made);
    made.catch(() => {
      if (this.images.get(image) === made) this.images.delete(image);
    });
    return made;
  }

  /** The registry's tarball of `version` of the package `name`, undefined when it has none. A try
   *  that failed for a transient reason (an NpmFailed whose `failure` names one) is sent again as
   *  `retries` says, as a gh read is, and the read fails after its last try naming each try's
   *  failure; any other failure fails it at once. Nothing is kept: each call reads the registry. */
  async tarball(name: string, version: string): Promise<Uint8Array | undefined> {
    // A try is one `GhRetries.send` reads by its code and its error text, the bytes beside them.
    const read = await this.retries.send(
      async () => {
        try {
          return { code: 0, stderr: "", bytes: await this.npm(name, version) };
        } catch (error) {
          if (!(error instanceof NpmFailed)) throw error;
          return { code: 1, stderr: error.failure, bytes: undefined };
        }
      },
      { read: `the registry read of ${tarballUrl(name, version)}` }
    );
    if (read.code !== 0) throw new NpmFailed(tarballUrl(name, version), firstLine(read.stderr));
    return read.bytes;
  }

  /** The names of the skills the registry's tarball of `version` of the package `name` ships
   *  (copies.ts's `tarballSkills`), undefined when the registry has none (`tarball`). Kept for
   *  each version, since the registry takes no version twice; a read that fails is not kept, so
   *  a later caller reads the registry again. */
  shippedSkills(name: string, version: string): Promise<ReadonlySet<string> | undefined> {
    const key = `${name}@${version}`;
    const held = this.shipped.get(key);
    if (held !== undefined) return held;
    const made = this.tarball(name, version).then((bytes) =>
      bytes === undefined ? undefined : new Set(tarballSkills(tarballFiles(bytes)))
    );
    this.shipped.set(key, made);
    made.catch(() => {
      if (this.shipped.get(key) === made) this.shipped.delete(key);
    });
    return made;
  }

  /** Whether GitHub lists the tag `tag` of `repo`: one read of its tags that start so. */
  async hasTag(repo: string, tag: string): Promise<boolean> {
    const refs = await this.ok([
      "api",
      `repos/${repo}/git/matching-refs/tags/${encodeURIComponent(tag)}`,
      "--jq",
      ".[].ref",
    ]);
    return refs.split("\n").includes(`refs/tags/${tag}`);
  }

  /** gh's answer to `args`, sent again as `retries` says, the read made once: an answer that
   *  failed for a transient reason on every try (`transientOf`) is dropped once it does. */
  private call(args: readonly string[]): Promise<Sent<GhResult>> {
    const key = args.join("\0");
    const held = this.made.get(key);
    if (held !== undefined) return held;
    const made = this.send(args);
    this.made.set(key, made);
    const drop = () => {
      if (this.made.get(key) === made) this.made.delete(key);
    };
    made.then((result) => {
      if (result.code !== 0 && transientOf(result) !== undefined) drop();
    }, drop);
    return made;
  }

  /** gh's answer to `args`, sent again as `retries` says: after a transient failure, and, while
   *  GitHub refuses it for its rate limit, after the wait common.ts's `installationRateWait` gives
   *  (a primary limit waited out to the reset of the installation that refused it), as copies.ts's
   *  and context.ts's reads are. */
  private send(args: readonly string[], splits = false) {
    return this.retries.send(() => this.gh(args), {
      splits,
      rateWait: installationRateWait(args, {
        gh: (read) => this.gh(read),
        say: (line) => this.retries.say(line),
      }),
    });
  }

  private async ok(args: readonly string[]): Promise<string> {
    return ghAnswer(args, await this.call(args));
  }

  /** The default branch of `repo`: repositories.tsv's (`from` LISTED_BRANCH), else GitHub's
   *  (`githubBranch`). */
  async branch(repo: string): Promise<{ branch: string; from: string }> {
    const known = this.branches.get(repo);
    if (known !== undefined && known !== "") return { branch: known, from: LISTED_BRANCH };
    return this.githubBranch(repo);
  }

  /** The default branch GitHub names of `repo`. */
  async githubBranch(repo: string): Promise<{ branch: string; from: string }> {
    const branch = (await this.ok(["api", `repos/${repo}`, "--jq", ".default_branch"])).trim();
    if (branch === "") throw new Error(`GitHub names no default branch of ${repo}`);
    return { branch, from: `gh api repos/${repo}` };
  }

  /** The last commit of `branch` of `repo` at or before `at`: one GET whose query is in its path,
   *  as copies.ts reads a commit, which the pod's gh sends for a repository of any owner, a pinned
   *  one outside the configured owners too (the same GET given as `-X GET` with `-f` fields it
   *  refuses for an owner it holds no GitHub App token of). Throws NoCommitThen when GitHub gives
   *  none. */
  async commitAt(repo: string, branch: string, at: string): Promise<string> {
    const sha = (
      await this.ok([
        "api",
        `repos/${repo}/commits?sha=${encodeURIComponent(branch)}&until=${encodeURIComponent(at)}&per_page=1`,
        "--jq",
        ".[0].sha",
      ])
    ).trim();
    if (!/^[0-9a-f]{40}$/.test(sha))
      throw new NoCommitThen(`no commit of ${repo} ${branch} at or before ${at}`);
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
    return ghAnswer(args, result);
  }

  /** The commit that GitHub's build provenance of the image digest `digest` (sha256, hex) names
   *  in `repo`, one read of `repos/<repo>/attestations/sha256:<digest>`: of the first attestation
   *  whose in-toto statement names a git commit as its first resolved dependency (SLSA provenance
   *  v1's `buildDefinition.resolvedDependencies[0].digest.gitCommit`), that commit and the
   *  dependency's uri. Undefined when GitHub holds no attestation of the digest there (a 404) or
   *  none names a commit; throws when the read fails otherwise. */
  async attestedCommit(
    repo: string,
    digest: string
  ): Promise<{ readonly commit: string; readonly uri: string } | undefined> {
    const args = ["api", `repos/${repo}/attestations/sha256:${digest}`];
    const result = await this.call(args);
    if (result.code !== 0 && /\b404\b|Not Found/.test(result.stderr)) return undefined;
    const answer = Attestations.safeParse(parseJson(ghAnswer(args, result)));
    for (const attestation of answer.data?.attestations ?? []) {
      const payload = attestation.bundle?.dsseEnvelope?.payload;
      if (payload === undefined) continue;
      const statement = Provenance.safeParse(
        parseJson(Buffer.from(payload, "base64").toString("utf8"))
      );
      const [first] = statement.data?.predicate.buildDefinition.resolvedDependencies ?? [];
      const commit = first?.digest?.gitCommit;
      if (commit !== undefined && /^[0-9a-f]{40}$/.test(commit)) {
        return { commit, uri: first?.uri ?? "" };
      }
    }
    return undefined;
  }

  /** The entries of the tree `sha` of `repo` (a commit's id gives its root tree's): every entry
   *  below it when `recursive`, else those of its own level; and whether GitHub cut the answer
   *  short. The answer is not kept (a large repository's tree is megabytes); `at` and `project`
   *  keep what they read from it. */
  private async treeAnswer(
    repo: string,
    sha: string,
    recursive: boolean
  ): Promise<{ entries: TreeEntry[]; truncated: boolean }> {
    const args = ["api", `repos/${repo}/git/trees/${sha}${recursive ? "?recursive=1" : ""}`];
    const stdout = ghAnswer(args, await this.send(args));
    const tree = GitTree.safeParse(parseJson(stdout));
    if (!tree.success) throw new Error(`gh ${args.join(" ")} gave no git tree`);
    return { entries: tree.data.tree, truncated: tree.data.truncated === true };
  }

  /** Every entry of the tree of `repo` at `commit`. Throws on a tree GitHub cuts short. */
  private async tree(repo: string, commit: string): Promise<TreeEntry[]> {
    const { entries, truncated } = await this.treeAnswer(repo, commit, true);
    if (truncated) {
      throw new Error(`GitHub cuts the tree of ${repo} at ${commit} short, so it is not read`);
    }
    return entries;
  }

  /** The entries of one level of the tree `sha` of `repo`, the directory `dir` of it (`""` the
   *  root), each with its path in the repository. Throws when GitHub cuts even that level short. */
  private async level(repo: string, sha: string, dir: string): Promise<TreeEntry[]> {
    const { entries, truncated } = await this.treeAnswer(repo, sha, false);
    if (truncated) {
      throw new Error(
        `GitHub cuts ${dir === "" ? "the root" : dir} of ${repo} short even one level deep, so it is not read`
      );
    }
    return entries.map((entry) => ({
      ...entry,
      path: dir === "" ? entry.path : `${dir}/${entry.path}`,
    }));
  }

  /** The text of the blob `sha` of `repo`, as Oh My Pi reads a file (common.ts's `ompText`: a
   *  leading byte order mark dropped, whether the blob comes over REST or in a GraphQL batch,
   *  `prefetch`), read once: a blob's id is its content, so a file that two commits share is read
   *  for the first. */
  private blob(repo: string, sha: string): Promise<string> {
    return readOnce(this.blobs, sha, () =>
      this.ok(["api", `repos/${repo}/git/blobs/${sha}`]).then((answer) => {
        const blob = GitBlob.safeParse(parseJson(answer));
        if (!blob.success || blob.data.encoding !== "base64") {
          throw new Error(`gh api repos/${repo}/git/blobs/${sha} gave no base64 blob`);
        }
        return ompText(Buffer.from(blob.data.content, "base64"));
      })
    );
  }

  /** Reads the blobs `entries` of `repo` that are not read yet, in GraphQL batches of up to
   *  BLOB_BATCH blobs and BLOB_BATCH_BYTES bytes (copies.ts's `blobBatches` and `blobQuery`), one
   *  call a batch where each blob alone would take one, with the owner and name as the fields
   *  `owner` and `name` (`graphqlArgs`), by which the pod's gh picks the owner's token. A blob a
   *  batch does not give whole (binary, cut short, missing, or in a batch that fails: a batch of
   *  more than one blob that GitHub answers as a batch too large, a 502, a 504 or its refusal of a
   *  query too costly, is not sent again) is left to `blob`, which reads it alone. */
  private async prefetch(repo: string, entries: readonly TreeEntry[]): Promise<void> {
    const wanted = new Map<string, TreeEntry>();
    for (const entry of entries) {
      if (!this.blobs.has(entry.sha) && /^[0-9a-f]{40}$/.test(entry.sha)) {
        wanted.set(entry.sha, entry);
      }
    }
    for (const batch of blobBatches([...wanted.values()], BLOB_BATCH, BLOB_BATCH_BYTES)) {
      const result = await this.send(graphqlArgs(repo, blobQuery(batch)), batch.length > 1);
      const parsed = BlobBatch.safeParse(result.code === 0 ? parseJson(result.stdout) : undefined);
      if (!parsed.success) continue;
      for (const [index, entry] of batch.entries()) {
        const blob = parsed.data.data.repository?.[`b${index}`];
        if (blob?.isBinary === false && !blob.isTruncated && blob.text !== null) {
          this.blobs.set(entry.sha, Promise.resolve(ompText(blob.text)));
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
    return readOnce(this.held, `${repo}\0${commit}`, () => this.readAt(repo, commit, excludePaths));
  }

  private async readAt(
    repo: string,
    commit: string,
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<RepositoryAt> {
    return this.fromEntries(repo, await this.tree(repo, commit), excludePaths);
  }

  /**
   * What the session repository `repo` holds at `commit` where Oh My Pi reads a project for a
   * working directory in a checkout of it: `at`'s reading of the entries of the directories it
   * reads, one level of each a call by its tree id. Those are each of `levels` (the working
   * directory's path in the checkout, then each parent up to the root, `""`), each directory of
   * PROJECT_LEVEL_DIRS there, each skills directory of the PROJECT_SKILLS kinds there, and each
   * skill directory in it. A skill directory a skills directory holds as a symbolic link (git's
   * mode 120000) is read where the link's text, taken from the link's directory, names a
   * directory of the repository, its entries kept under the link's path, as Oh My Pi's scan
   * follows a link (PROJECT_SKILLS); a link out of the repository, or to no directory of it, is
   * left out (`links` names each link, followed or not). So a repository whose whole tree GitHub
   * cuts short is read too, and a large one in a few calls; for the paths the rebuild uses
   * (CONTEXT_FILES, PROJECT_SKILLS) it gives what `at` gives. Read once for each commit and
   * levels; gives its tree reads too.
   */
  project(
    repo: string,
    commit: string,
    levels: readonly string[],
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<ProjectRead> {
    return readOnce(this.projects, [repo, commit, ...levels].join("\0"), () =>
      this.readProject(repo, commit, levels, excludePaths)
    );
  }

  private async readProject(
    repo: string,
    commit: string,
    levels: readonly string[],
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<ProjectRead> {
    const entries = await this.level(repo, commit, "");
    const read = new Set([""]);
    /** Adds one level of each directory of `dirs` that the entries read so far hold as a tree
     *  and that is not read yet. */
    const readDirs = async (dirs: readonly string[]) => {
      const found = entries.filter(
        (entry) => entry.type === "tree" && dirs.includes(entry.path) && !read.has(entry.path)
      );
      for (const entry of found) read.add(entry.path);
      const answers = await mapLimit(found, GITHUB_JOBS, (entry) =>
        this.level(repo, entry.sha, entry.path)
      );
      for (const answer of answers) entries.push(...answer);
    };
    const under = (level: string, dir: string) => (level === "" ? dir : `${level}/${dir}`);
    // From the root down to the working directory: each level is a directory of the one above.
    const down = [...levels].reverse();
    for (const level of down) await readDirs([level]);
    const missing = levels.filter((level) => !read.has(level));
    await readDirs(down.flatMap((level) => PROJECT_LEVEL_DIRS.map((dir) => under(level, dir))));
    const skillDirs = down.flatMap((level) =>
      PROJECT_SKILLS.flatMap((kind) => kind.dirs.map((dir) => under(level, dir)))
    );
    await readDirs(skillDirs);
    await readDirs(
      entries
        .filter((entry) => skillDirs.includes(path.posix.dirname(entry.path)))
        .map((entry) => entry.path)
    );
    const links: ProjectLink[] = [];
    const linked = entries.filter(
      (entry) => entry.mode === "120000" && skillDirs.includes(path.posix.dirname(entry.path))
    );
    for (const link of linked) {
      const target = await this.blob(repo, link.sha);
      const resolved = linkTargetPath(link.path, target);
      if (resolved === undefined) {
        links.push({ link: link.path, target, outcome: "outside" });
        continue;
      }
      // The target's directories from the root down, each read as the entries above give it.
      const parts = resolved.split("/");
      for (let depth = 1; depth <= parts.length; depth++) {
        await readDirs([parts.slice(0, depth).join("/")]);
      }
      if (!read.has(resolved)) {
        links.push({ link: link.path, target, outcome: "no directory" });
        continue;
      }
      links.push({ link: link.path, target, outcome: "followed" });
      for (const child of entries.filter((each) => path.posix.dirname(each.path) === resolved)) {
        entries.push({ ...child, path: `${link.path}/${path.posix.basename(child.path)}` });
      }
    }
    return {
      at: await this.fromEntries(repo, entries, excludePaths),
      reads: read.size,
      links,
      missing,
    };
  }

  /** The first of `files`, paths in the repository, that the tree of `repo` at `commit` holds as a
   *  file (a blob, a symbolic link too), or undefined when it holds none of them: read from the
   *  root down, one level of each directory on the way to a file a call by its tree id, each
   *  directory of a commit read once. */
  async firstFile(
    repo: string,
    commit: string,
    files: readonly string[]
  ): Promise<string | undefined> {
    for (const file of files) {
      let entry: TreeEntry | undefined = { path: "", mode: "040000", type: "tree", sha: commit };
      for (const part of file.split("/")) {
        if (entry?.type !== "tree") {
          entry = undefined;
          break;
        }
        const dir: TreeEntry = entry;
        const level = await readOnce(this.dirs, [repo, commit, dir.path].join("\0"), () =>
          this.level(repo, dir.sha, dir.path)
        );
        const name = dir.path === "" ? part : `${dir.path}/${part}`;
        entry = level.find((each) => each.path === name);
      }
      if (entry?.type === "blob") return file;
    }
    return undefined;
  }

  /** What `repo` holds by the tree entries `entries`, as `at` reads it (see there). A SKILL.md or
   *  manifest whose read failed is a fault of the result, unless it failed for a transient reason
   *  on every try: then this rejects with that failure. */
  private async fromEntries(
    repo: string,
    entries: readonly TreeEntry[],
    excludePaths: SkillCutsConfig["excludePaths"]
  ): Promise<RepositoryAt> {
    const faults: { file: string; why: string }[] = [];
    // No fork is read here: the entries kept are those of a repository that is no fork.
    const { skillFiles: skillEntries, manifests: candidates } = repositoryFiles(
      repo,
      entries.filter((entry) => keepsEntry(entry, false)),
      excludePaths
    );
    await this.prefetch(repo, [...skillEntries, ...candidates.map(({ entry }) => entry)]);
    const hidden = new Set<string>();
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
        const text = await this.blob(repo, entry.sha);
        if (skillTextFrontmatter(text).hidden) hidden.add(dir);
        return { ...base, ...skillFields(dir, repo, text) };
      } catch (error) {
        // A read that failed for a transient reason on every try fails the whole read, which is
        // then not kept (`readOnce`), so a later caller reads it again; any other is a fault.
        if (transientFailure(error)) throw error;
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
          if (transientFailure(error)) throw error;
          faults.push({ file: entry.path, why: `${kind}: ${messageFor(error)}` });
          return { ...base, facts: [], ofKind: true, says: "", error: messageFor(error) };
        }
      }
    );
    const contextSizes = new Map<string, number>();
    const ompLevels = new Set<string>();
    for (const entry of entries) {
      const parts = entry.path.split("/");
      const omp = parts.indexOf(".omp");
      if (omp >= 0 && (omp < parts.length - 1 || entry.type === "tree")) {
        ompLevels.add(parts.slice(0, omp).join("/"));
      }
      if (
        entry.type === "blob" &&
        CONTEXT_FILES.some(({ file }) => entry.path === file || entry.path.endsWith(`/${file}`))
      ) {
        // GitHub gives each blob's size; a blob given none is taken to hold text.
        contextSizes.set(entry.path, entry.size ?? 1);
      }
    }
    return { copies, manifests, faults, contextSizes, ompLevels, hidden };
  }
}

/** The profile's GitHub reads with `io`'s gh: a read that failed for a transient reason waits with
 *  `io.sleep` before it is sent again, and is said on `io.err` as `who`'s. An image's config is
 *  read with `io.imageConfig`, else from its registry with the registries `config` names, each
 *  request sent again by the same rule and counted with the GitHub reads' retries. */
function githubReads(
  io: BuildIo,
  config: SkillCutsConfig,
  branches: ReadonlyMap<string, string>,
  who: "build-cases" | "build-tasks"
): GithubReads {
  const retries = new GhRetries(
    { sleep: (ms) => io.sleep?.(ms) ?? Bun.sleep(ms), err: (line) => io.err(line) },
    who
  );
  const registries = config.registries ?? [];
  const imageConfig =
    io.imageConfig ?? ((image: string) => registryImageConfig(image, { registries, retries }));
  return new GithubReads(io.gh, branches, retries, io.npm, imageConfig);
}

/** Which skills a package pinned by an exact npm version ships, and where its source says so. */
const NPM_PIN_RULE =
  "a plugin list that pins a package by an exact npm version installs that version (bun install), which ships the skills of that release: the package's release tag (<directory>-v<version> of its repository, v<version> for a package at its repository's root) when GitHub lists it and its tree holds the package, read as copies.ts reads a package; else the registry's tarball of the version, which names the skills it ships (copies.ts's tarballSkills); a version of neither ships nothing to the profile";

/** How a package pinned by an npm version is read (NPM_PIN_RULE): at its release tag (`sha`,
 *  and the rows by which it ships each skill there), else the names its tarball ships, else
 *  neither (left out); with why. */
interface PinnedVersion {
  readonly why: string;
  readonly sha?: string;
  readonly rows?: ReturnType<typeof resolveInstallers>["rows"];
  readonly names?: ReadonlySet<string>;
}

/** The session repository: the repository the session's working directory is a checkout of
 *  (`sessionRepository`), or undefined; the working directory's path inside that checkout (`rel`,
 *  `""` at its root); the evidence of it; and, where a parent's name gave it, that parent
 *  (`byParent`), whose repository's tree must hold `rel` before it is taken, else, for a `rel` of
 *  one directory, a file the session read under the working directory, which makes the working
 *  directory a workspace of the repository at its root (`caseProfile`). */
interface Checkout {
  readonly repo: string | undefined;
  readonly rel: string;
  readonly line: Evidence;
  readonly byParent?: string;
}

/** What the profile of one case is built from: its store, its context (the store's, for a
 *  subagent) and project, each successful read result of its messages whose first line is
 *  `[Skill file: <path>]` (any skill's), each of its successful reads by an absolute path (any
 *  file's), the session's start, its working directory, whether its session recorded its own
 *  system prompt (a subagent's), which holds its context files, and the session repository when
 *  it is known already (`--reprofile` of a case whose session the store no longer holds: the old
 *  case's evidence of it), else it is found from the working directory. */
interface ProfileInput {
  readonly store: string;
  readonly context: string;
  readonly ompProfile: string;
  readonly project: string | undefined;
  readonly reads: readonly ShownRead[];
  /** Each successful read of its messages by an absolute path (`absoluteReads`): a file of them
   *  under the working directory that the session repository's tree holds at its path there makes
   *  a working directory directly inside a parent named as that repository a workspace of it
   *  (DEVBOX_CHECKOUT_RULE). */
  readonly absoluteReads: readonly AbsoluteRead[];
  readonly sessionStart: string;
  readonly cwd: string | undefined;
  readonly recordedPrompt: boolean;
  readonly checkout?: Checkout;
  /** When its repositories are read: a subagent's at its root's last prompt build before its
   *  start (PARENT_LOAD_RULE), a top-level session's at the build its step ran under
   *  (TOP_BUILD_RULE); every one at the session's start when absent. */
  readonly readTime?: ReadTime;
  /** The recorded listing its listing measure sets it against (`decidingListing`): the session's
   *  own recorded prompt's (a subagent's), or that of the subagent the measure uses for a session
   *  that records only its prompt's digest. It decides each pool (POOL_LISTING_RULE). */
  readonly recordedListing?: DecidingListing;
  /** The Oh My Pi version the session's file records it ran (`runtimeOf`), which the pool version
   *  rule sets beside the dev box's pin (POOL_VERSION_RULE). */
  readonly runtime?: string | undefined;
  /** Every pool is installed, by no rule of the two: a top-level session's profile at the builds
   *  a step in the case reader's reach can run under, before its step's build and the subagent
   *  its listing measure uses are known, which must install all that the step's profile can. */
  readonly poolsUndecided?: true;
}

/** A recorded listing that decides a profile's pools (POOL_LISTING_RULE): the names it lists, and
 *  where it was recorded, in words. */
interface DecidingListing {
  readonly names: readonly string[];
  readonly from: string;
}

/** How a dev-box session's repository is found from its working directory. */
const DEVBOX_CHECKOUT_RULE =
  "a dev-box session's repository is the dev box's own repository (DEVBOX_INSTALLERS_RULE) where the working directory is in its checkout, the directory whose .claude/ the configuration's devboxClaudeConfigDir names as the dev box's CLAUDE_CONFIG_DIR; else the one repository of the copies output's list (repositories.tsv) named as the last part of its working directory, or of the nearest of that directory's parents named as any listed repository when that repository's tree at its commit holds the working directory's path below that parent: a parent's name alone decides nothing; where the tree does not hold that path and the working directory is directly inside that parent, the working directory is a workspace of that repository (a jj or git worktree) and the checkout's root when a file the session read by an absolute path under the working directory is a file of the tree at its path below the working directory";
/** Which units a dev-box profile installs beside those its session's reads come from. */
const DEVBOX_INSTALLERS_RULE =
  "a dev box installs every unit the installers of its own repository install into Oh My Pi at the session's start, whether or not the session read from it: the one repository of the copies output (manifests.tsv) that holds both an installer script (installers/*.sh) and Claude Code's settings (.claude/settings.json), whose .claude/ is the dev box's Claude Code config directory, read at its commit of the session's start with each repository its installers pin, and resolved by copies.ts's rules (resolveInstallers)";
/** Which context a session's profile is built in, and which of its reads place units there. */
const SESSION_CONTEXT_RULE =
  "a session's context follows its store, its Oh My Pi profile (its transcript's path: <home>/.omp/agent/ for the default profile, <home>/.omp/profiles/<name>/agent/ for a named one) and where its skills came from, which a [Skill file: <path>] header shows, since Oh My Pi prints one only for a skill it loaded. There are five kinds. A default-profile session of a devbox store ran on the dev box, which installs its own repository's units with their pins at the session's start (DEVBOX_INSTALLERS_RULE). A named-profile session of a devbox store with at least one such header under the dev box's home (the configuration's devboxHome, matched by whole path segments; no session has this kind where the configuration names none) ran in a pod that mounts that home, whose Oh My Pi loaded the dev box's skills, so it gets the dev box's context too. Any other named-profile session of a devbox store ran in a session pod, which links the packages of its pinned image into the profile. A legion-profile session of a Legion store ran in a Legion pod, which installs its worker image's packages and its deployment's role setup. Any other named-profile session of a Legion store gets its session repository's project skills. In every context each such header places its unit at the copy its path resolves to (the prefix table, else attribute.ts's rules), which is direct proof, and a unit the context installs already is not added again; a whole read of a file by its path places a unit only in a dev box's context, a Legion pod's legion profile or a session whose profile is not known, since in a pod its links, not the files a session read, decide which units it ships";
/** How Oh My Pi 18.18.6 reads skills.ignoredSkills. */
const IGNORED_RULE =
  "Oh My Pi 18.18.6 loads no skill whose name a pattern of skills.ignoredSkills matches as a Bun.Glob, matched against the skill's own name and again against the name it is listed under, <namespace>/<name> for a later copy of a name whose text differs (extensibility/skills.ts, lines 386-389, 401-406, 437 and 544), so a session can neither list nor read it";
/** Which pools an extension's resources_discover gives a profile installs, where the session
 *  records a listing. */
const POOL_LISTING_RULE =
  "Oh My Pi 18.18.6 lists the skills of a pool an extension gives through resources_discover (a pool of the dev box's skills-sources.json) only where it loaded that extension's pools and the pool's clone was there then: a top-level session as it starts and on each reload that rebuilds its prompt, a subagent as it starts, merging what it gets into the snapshot its parent gave it (session/session-tools.ts, lines 2321-2337, 2466-2506 and 2530-2547, and #mergeDiscoveredSkillDirectories, lines 2350-2420); dev-box sessions that ran one Oh My Pi build list these pools in some sessions and not in others, and no file records why, nor which clones the host had then. So where the session records a listing (its own recorded prompt's, or that of the subagent its listing measure uses), that listing decides each pool, an inference from the listing: the profile installs a pool where the listing names at least one of the skills the pool holds at the session's start that no installed unit outside the pools also ships (a name such a unit ships is no evidence for the pool, since the session lists it whether or not the pool is there), and leaves it out where the listing names no such skill; a pool the listing names in part is installed whole, and the listing measure counts its unnamed skills as added names. A pool that a [Skill file: …] read of the session places is installed whatever the listing names, since that read shows Oh My Pi loaded it, which is direct proof and no inference from the listing. The measure marks each name of a pool installed on the listing (fromRecorded), since there the recorded listing both installs the names and is what the measure compares against; it marks no name of a pool installed on a read alone";
/** Which pools a profile installs where the session records no listing. */
const POOL_VERSION_RULE =
  "where the session records no listing and no subagent's listing measures it, the Oh My Pi build its dev box ran decides the pools: the version the dev box's own repository pins (the configuration's ompPin: a TOML file of that repository and the keys to the version in it), read at its last commit at or before the session's start, set against ompPin's firstPoolVersion, the first build that loads an extension's resources_discover pools on a fresh start (an earlier build deferred an extension whose only registration is resources_discover, so a fresh session listed none of its pools); a pin before that version installs no pool but one a [Skill file: …] read of the session places, which shows Oh My Pi loaded it, and a pin at or after it installs each pool, both inferences from the pinned build. The Oh My Pi version the session's file records (the runtime.version of its first model_change line; a fork's build gives its release alone) is set beside the pin as a cross-check and decides nothing. With no ompPin, or no version the file holds at those keys, each pool is installed as the dev box's installers install it";
/** Which Claude Code plugins Oh My Pi lists for a session. */
const CLAUDE_PLUGIN_RULE =
  "Oh My Pi 18.18.6 lists the skills of a Claude Code plugin that Claude Code's registry installs for the project: a project- or local-scope install recorded for the active project, or one that enabledPlugins of the project's .claude/settings.json (or settings.local.json, or the config directory's settings.json) turns on (discovery/helpers.ts, readClaudeEnabledPlugins, lines 1310-1333, and loadClaudePluginRoots, lines 1494-1570), at priority 70, a second copy of a name under the plugin's name (discovery/claude-plugins.ts, lines 36 and 226-275); a user-scope install only where Oh My Pi's enabledProviders names claude-plugins or claude (extensibility/skills.ts, lines 361-362; capability/index.ts, lines 419-429), which no profile config read here sets. Claude Code installs the plugins a project's .claude/settings.json enables for that project, so a session in a checkout of the repository that holds those settings lists them; the registry itself is host state that no repository holds";

/** The dev box's repository by DEVBOX_INSTALLERS_RULE, or undefined, with the evidence of why,
 *  when no repository or more than one holds both. */
function devboxHome(copies: Copies, evidence: Evidence[]): string | undefined {
  const homes = devboxRepositories(copies.manifests);
  const [home] = homes;
  if (homes.length === 1 && home !== undefined) return home;
  evidence.push({
    item: "left out: the dev box's installers",
    source: `${DEVBOX_INSTALLERS_RULE}; ${homes.length === 0 ? "no repository holds both" : `${homes.length} repositories hold both (${homes.join(", ")})`}, so the profile installs only the units the session's reads come from`,
  });
  return undefined;
}

/** The items of a profile's evidence that name the context of a pod that mounts the dev box's
 *  home (SESSION_CONTEXT_RULE), and that of such a pod's session that read skill files outside
 *  that home too, each followed by the profile's name. */
const MOUNTED_HOME_ITEM = "a pod that mounts the dev box's home, profile ";
const MIXED_HEADERS_ITEM = "skill files from the dev box's home and the pod's own paths, profile ";

/** Whether the absolute path `file` is in the directory `dir` by whole path segments:
 *  `/home/dev2/x` is not in `/home/dev`. */
function inDirectory(file: string, dir: string): boolean {
  const below = path.posix.relative(dir, file);
  return path.posix.isAbsolute(file) && below !== ".." && !below.startsWith("../");
}

/** The evidence that a session of the named profile `profile` in a devbox store ran in a pod that
 *  mounts the dev box's home `home` (SESSION_CONTEXT_RULE): `headers`, its `[Skill file: …]`
 *  reads, hold one or more under that home. With a second item, naming a header of each kind,
 *  where some are outside it. Undefined where none is under it. */
function mountedHome(
  profile: string,
  home: string,
  headers: readonly ShownRead[]
): Evidence[] | undefined {
  const inside = headers.filter((read) => inDirectory(read.header, home));
  const [first] = inside;
  if (first === undefined) return undefined;
  const outside = headers.find((read) => !inDirectory(read.header, home));
  const one = inside.length === 1;
  const mounted = {
    item: `${MOUNTED_HOME_ITEM}${profile}`,
    source: `${inside.length} [Skill file: …] header${one ? "" : "s"} of the session ${one ? "is" : "are"} under the dev box's home ${home} (the configuration's devboxHome), the first at message ${first.index}: ${first.header}; Oh My Pi prints such a header only for a skill it loaded, so the pod's Oh My Pi loaded the dev box's skills, and the profile is the dev box's at the session's start: ${SESSION_CONTEXT_RULE}`,
  };
  if (outside === undefined) return [mounted];
  return [
    mounted,
    {
      item: `${MIXED_HEADERS_ITEM}${profile}`,
      source: `message ${first.index} reads [Skill file: ${first.header}], under the dev box's home ${home}, and message ${outside.index} reads [Skill file: ${outside.header}], outside it; the profile is the dev box's, and each header outside that home places its unit too, which is direct proof: ${SESSION_CONTEXT_RULE}`,
    },
  ];
}

/** The item of a profile's evidence that names the Oh My Pi build the dev box's repository pins
 *  (POOL_VERSION_RULE), followed by that repository. */
const OMP_PIN_ITEM = "Oh My Pi pin of ";

/** The release of the version `version` (`18.16.0` of `v18.16.0-fork.1+b`): a fork's build gives
 *  its runtime version so. */
function releaseOf(version: string): string {
  return version.replace(/^v/, "").split(/[-+]/)[0] ?? "";
}

/** The Oh My Pi build the dev box's repository `home` pins at the session's start `start`, by the
 *  configuration's ompPin `pin` (POOL_VERSION_RULE): the value at its keys in its TOML file, read
 *  at the last commit of the repository's branch at or before `start`, when it is a version; and
 *  what was read, in words. No `version` where the repository has no commit then, the file is not
 *  there or not TOML, or it holds no version at those keys. Throws when a GitHub read fails. */
async function pinnedOmp(
  github: GithubReads,
  home: string,
  pin: NonNullable<SkillCutsConfig["ompPin"]>,
  start: string
): Promise<{ readonly version?: string; readonly read: string }> {
  const keys = pin.key
    .map((key) => (/^[A-Za-z0-9_-]+$/.test(key) ? key : JSON.stringify(key)))
    .join(".");
  const { branch } = await github.branch(home);
  let sha: string;
  try {
    sha = await github.commitAt(home, branch, start);
  } catch (error) {
    if (!(error instanceof NoCommitThen)) throw error;
    return { read: `${home} has no commit of ${branch} at or before the session's start ${start}` };
  }
  const read = `${home}:${pin.file} at ${sha} (the last commit of ${branch} at or before the session's start ${start})`;
  const text = await github.text(home, sha, pin.file);
  if (text === undefined) return { read: `${read} does not exist` };
  let value: unknown;
  try {
    value = Bun.TOML.parse(text);
  } catch (error) {
    return { read: `${read} is no TOML file (${messageFor(error)})` };
  }
  for (const key of pin.key) value = isObject(value) ? value[key] : undefined;
  if (typeof value !== "string" || !OMP_VERSION.test(value)) {
    return {
      read: `${read} pins no version at ${keys}${typeof value === "string" ? ` (it holds ${value})` : ""}`,
    };
  }
  return { version: value, read: `${read} pins ${keys} at ${value}` };
}

/** What a Legion pod installs beside its worker image's packages. */
const LEGION_INSTALLS_RULE =
  "a Legion pod's role start runs its deployment's role setup (a legion/*setup.sh of the repository whose project file names the project), which installs, from the catalog repository it names (omp plugin marketplace add, then omp plugin install <plugin>@<catalog>), the every-role plugins its program names (everyRolePlugins in legion/main.go) and, when the project file sets verificationSkillsLeaveOut (an empty list too), every other plugin of the catalog but those it names; its worker image (legion/worker*/Dockerfile) installs each skill of a release as an Oh My Pi plugin under /opt; the deployment and the catalog are read at the session's start. A daemon-launched controller installs the every-role plugins alone, which a profile does not tell apart";

/** A marketplace's plugin entry, as copies.ts reads one. */
type MarketplacePlugin = Extract<Manifest["facts"][number], { type: "marketplace-plugin" }>;

/** What a Legion deployment's role setup installs for the project whose file is `projectPath`
 *  of `deployment` (LEGION_INSTALLS_RULE), from `manifests` (the deployment's and its catalog's at
 *  the session's start): the catalog pins of its setup, each plugin entry of the catalog's
 *  marketplace, the plugins the project's roles install, and why. */
function legionCatalog(
  manifests: readonly Manifest[],
  deployment: string,
  projectPath: string
): {
  setup: InstallerPin[];
  plugins: { manifest: Manifest; fact: MarketplacePlugin }[];
  wanted: Set<string>;
  why: string;
} {
  const own = manifests.filter((manifest) => manifest.repository === deployment);
  const setup = factsOf(own, "role-setup").map(({ manifest, fact }) => ({
    repository: fact.repo,
    ref: "",
    manifest,
  }));
  const everyRole = factsOf(own, "every-role").flatMap(({ fact }) => fact.plugins);
  const leaveOut = own
    .find((manifest) => manifest.path === projectPath)
    ?.facts.flatMap((fact) => (fact.type === "leaves-out" ? [fact.plugins] : []))[0];
  const plugins = factsOf(
    manifests.filter((manifest) =>
      setup.some((pin) => pin.repository.toLowerCase() === manifest.repository.toLowerCase())
    ),
    "marketplace-plugin"
  );
  const wanted = new Set(everyRole);
  if (leaveOut !== undefined) {
    for (const { fact } of plugins) if (!leaveOut.includes(fact.plugin)) wanted.add(fact.plugin);
  }
  const every = everyRole.join(", ") || "none";
  return {
    setup,
    plugins,
    wanted,
    why:
      leaveOut === undefined
        ? `${projectPath} sets no verificationSkillsLeaveOut, so each role installs the every-role plugins (${every}) alone`
        : `${projectPath} sets verificationSkillsLeaveOut [${leaveOut.join(", ")}], so each role installs every plugin of the catalog but those, and the every-role plugins (${every})`,
  };
}

/** Which commit of its repository a Legion pod's worker image carries. */
const LEGION_IMAGE_RULE = `a Legion pod runs the worker image that the deployment names for its project at the deployment's commit of the session's start: the top-level image: line of the project file, else the image: line of the project's entry in the deployment's Pulumi config (Pulumi.prod.yaml in the directory above the project file's, the entry - key: <project>); that image carries the packages of its repository (the prefix-table.tsv rows under ${IMAGE_ROOT}) as the repository was at the commit the image was built from: the commit that the build provenance of the image's digest names (gh api repos/<repository>/attestations/sha256:<digest>, the gitCommit of its first resolved dependency); for an image whose digest has no such provenance there, the image itself decides, by the labels of its OCI config (read from the registry by digest, the config blob alone and no layer: org.opencontainers.image.revision, a commit of the repository, else org.opencontainers.image.version, mapped to the release tag v<version> or <name>-v<version> that the repository has); failing that, for an image of a repository that the configuration's deploymentImages names (an image the deployment builds on a worker image), the base it was built on decides: its OCI config's org.opencontainers.image.source label names the deployment and its org.opencontainers.image.revision label a commit of the deployment at or before its read, the Dockerfile the entry names at that commit builds its final stage on a digest-pinned base, and that base's rootfs.diff_ids are the first of the image's (a Dockerfile's ARG default is no proof of the base, since a build can pass another; the diff ids are the digests of the layers uncompressed, and the manifests' layer digests are not compared, since they differ where the build compressed a base layer again); the image's commit is then the base's, by the build provenance of its digest, else by its labels, and where any of these steps fails the image's commit is not known; for any other image, failing the labels, the comment lines right above its image: line decide, by the first of their words, in their order, that names a commit: an image digest whose provenance names one (the base the image was built on), a commit of the repository (a word of 7 to 40 hex digits, letters and digits both), or a tag of the repository (a word of the form <name>-v<version> or v<version>, at the tag's last commit at or before the deployment's read); where none of these gives a commit, the image's commit is not known, the repository's default branch is a guess that no profile is built on, and the case or task is not measurable`;

/** At which commit of the worker image's repository a Legion pod's daemon composed its role
 *  prompt (context.ts reads the item DAEMON_IMAGE_ITEM that names it). */
const LEGION_DAEMON_RULE = `the Legion daemon of a pod composes its role prompt from ${PROMPTS_GO} and the parts it embeds as they were at the commit its daemon image was built from; the deployment names that image for the project as it names the worker image, in a daemonImage: line (the project file's top-level one, else the one of the project's entry in the Pulumi config), and the image is built from the worker image's repository; its commit is decided by the worker image's rule (the build provenance of its digest, else the first word of the comment lines right above its daemonImage: line that names a commit); where neither gives a commit, the role prompt is composed at the commit the profile reads that repository's plugins at`;

/** A line of a project's values that names an image: its worker image (`image`) or its daemon
 *  image (`daemonImage`). */
type ImageKey = "image" | "daemonImage";

/** A YAML value less a quote at its start and one at its end. */
const unquoted = (value: string) => value.replace(/^["']|["']$/g, "");

/** The comment lines right above the line `at` of `lines`, none before the line `floor`, each less
 *  its `#`. */
function commentAbove(lines: readonly string[], at: number, floor: number): string[] {
  const comment: string[] = [];
  for (let above = at - 1; above >= floor && /^\s*#/.test(lines[above] ?? ""); above--) {
    comment.unshift((lines[above] ?? "").trim().replace(/^#\s?/, ""));
  }
  return comment;
}

/** The `key:` value of the entry `- key: <project>` of the Pulumi config text `text`, and the
 *  comment lines right above that line, each less its `#`; undefined when the text holds no such
 *  entry with a `key:` line. The entry's lines are those after its `- key:` line that are
 *  indented past its dash, or are comments; its `key:` is one of its own fields (indented two
 *  past the dash), so neither a nested map's `image:` nor another key such as `daemonImage:` is
 *  its `image:`. */
function projectImage(
  text: string,
  project: string,
  key: ImageKey
): { readonly image: string; readonly comment: readonly string[] } | undefined {
  const field = new RegExp(`^\\s*${key}:\\s*(\\S+)\\s*(?:#.*)?$`);
  const lines = text.split("\n");
  const head = lines.findIndex((line) => {
    const name = /^\s*- key:\s*(\S+)\s*$/.exec(line)?.[1];
    return name !== undefined && unquoted(name) === project;
  });
  if (head < 0) return undefined;
  const dash = (lines[head] ?? "").indexOf("-");
  for (let at = head + 1; at < lines.length; at++) {
    const line = lines[at] ?? "";
    if (line.trim() === "" || /^\s*#/.test(line)) continue;
    const indent = line.length - line.trimStart().length;
    if (indent <= dash) return undefined;
    const value = field.exec(line)?.[1];
    if (value === undefined || indent !== dash + 2) continue;
    return { image: unquoted(value), comment: commentAbove(lines, at, head + 1) };
  }
  return undefined;
}

/** The top-level `key:` value of a project file's text `text` (a deployment's
 *  `legion/projects/<n>-<project>.yaml`, where a project's own values moved from the Pulumi
 *  config's `- key: <project>` entry), and the comment lines right above that line, each less its
 *  `#`; undefined when the file has no top-level `key:` line. A nested map's `image:` and another
 *  key such as `daemonImage:` are not its `image:`. */
function projectFileImage(
  text: string,
  key: ImageKey
): { readonly image: string; readonly comment: readonly string[] } | undefined {
  const field = new RegExp(`^${key}:\\s*(\\S+)\\s*(?:#.*)?$`);
  const lines = text.split("\n");
  const at = lines.findIndex((line) => field.test(line));
  if (at < 0) return undefined;
  const value = field.exec(lines[at] ?? "")?.[1] ?? "";
  return { image: unquoted(value), comment: commentAbove(lines, at, 0) };
}

const OCI_REVISION = "org.opencontainers.image.revision";
const OCI_VERSION = "org.opencontainers.image.version";
const OCI_SOURCE = "org.opencontainers.image.source";

/**
 * The commit of `imageRepo` that the OCI config of the image `image` (a digest-pinned reference;
 * `GithubReads.imageConfig` reads its manifest and config blob from the registry, a public GHCR
 * image's with the anonymous pull token GHCR gives, a configured registry's with its token, and
 * pulls no layer) names: the commit of its `org.opencontainers.image.revision` label that
 * `imageRepo` holds at or before `when.at`; else the last commit at or before `when.at` of the
 * release tag its `org.opencontainers.image.version` label names (`v<version>`, or
 * `<name>-v<version>` for the first words of the image's name, the longest first), the first of
 * them that `imageRepo` has. `how` says what was read, and, with no `sha`, why no label decided;
 * it is empty when the registry gave no config (a host neither GHCR nor configured, or no pull).
 * A commit or tag that `imageRepo` does not have, such as a label that names another
 * repository's build, decides nothing.
 */
async function labelCommit(
  github: GithubReads,
  read: {
    readonly image: string;
    readonly imageRepo: string;
    readonly when: { readonly at: string; readonly words: string };
  }
): Promise<{ readonly sha?: string; readonly how: string }> {
  const { image, imageRepo, when } = read;
  const config = await github.imageConfig(image);
  if (config === undefined) return { how: "" };
  const found = `the OCI config of ${image}, read from its registry by digest,`;
  const revision = config.labels[OCI_REVISION];
  const version = config.labels[OCI_VERSION];
  /** The commit of `imageRepo` that `ref` (a commit or a tag) names, or undefined when it names
   *  none there at or before `when.at`. */
  const resolve = async (ref: string) => {
    try {
      return await github.commitAt(imageRepo, ref, when.at);
    } catch (error) {
      if (transientFailure(error)) throw error;
      return undefined;
    }
  };
  if (revision !== undefined && /^[0-9a-f]{40}$/.test(revision)) {
    const commit = await resolve(revision);
    if (commit === revision) {
      return {
        sha: commit,
        how: `${found} labels ${OCI_REVISION} ${revision}, a commit of ${imageRepo} (${commitsRead(imageRepo, revision, when.at)})`,
      };
    }
  }
  if (version !== undefined && version !== "") {
    const words = (
      image
        .replace(/@.*$/, "")
        .replace(/:[^/]*$/, "")
        .split("/")
        .pop() ?? ""
    ).split("-");
    const tags = [
      `v${version}`,
      ...words.map((_, at) => `${words.slice(0, words.length - at).join("-")}-v${version}`),
    ];
    for (const tag of new Set(tags)) {
      if (!(await github.hasTag(imageRepo, tag))) continue;
      const commit = await resolve(tag);
      if (commit === undefined) continue;
      return {
        sha: commit,
        how: `${found} labels ${OCI_VERSION} ${version}, so the release ${tag}, a tag of ${imageRepo} (gh api repos/${imageRepo}/git/matching-refs/tags/${tag}), whose last commit at or before ${when.words} ${when.at} is ${commit}`,
      };
    }
  }
  const labels = [OCI_REVISION, OCI_VERSION]
    .flatMap((name) =>
      config.labels[name] === undefined ? [] : [`${name} ${config.labels[name]}`]
    )
    .join(" and ");
  return {
    how: `${found} ${labels === "" ? "holds neither label" : `labels ${labels}, which name no commit or release tag of ${imageRepo}`}`,
  };
}

/**
 * The commit of `imageRepo` that the image `image` carries, where the configuration names its
 * repository as one the deployment `deployment` builds on a worker image (`deploymentImages`, the
 * entry `built`), by LEGION_IMAGE_RULE's deployment step: the image's OCI config labels
 * `org.opencontainers.image.source` the deployment (case aside, a trailing `/` aside) and
 * `org.opencontainers.image.revision` a commit that `deployment` holds at or before `when.at`; the
 * Dockerfile `built.dockerfile` at that commit builds its final stage on a digest-pinned base
 * (pod-image.ts's `finalStage`); the base's `rootfs.diff_ids` are the first of the image's
 * (`builtOn`: a build can pass the Dockerfile another base than its ARG default); and the commit
 * is the base's, by its build provenance in `imageRepo`, else by its labels (`labelCommit`). `how`
 * names each read, or, with no `sha`, the step that failed and why. Throws when a GitHub read
 * fails other than with a 404 of the Dockerfile or of a provenance.
 */
async function deploymentCommit(
  github: GithubReads,
  read: {
    readonly image: string;
    readonly built: DeploymentImage;
    readonly deployment: string;
    readonly imageRepo: string;
    readonly when: { readonly at: string; readonly words: string };
  }
): Promise<{ readonly sha?: string; readonly how: string }> {
  const { image, built, deployment, imageRepo, when } = read;
  const step = `the configuration's deploymentImages names ${built.repository}, an image ${deployment} builds on a worker image`;
  const config = await github.imageConfig(image);
  if (config === undefined) {
    return { how: `${step}, but its registry gives no OCI config of ${image}` };
  }
  const source = config.labels[OCI_SOURCE];
  const revision = config.labels[OCI_REVISION];
  const deployed = `https://github.com/${deployment}`;
  if (source === undefined || revision === undefined) {
    return {
      how: `${step}, but the OCI config of ${image} has no ${source === undefined ? OCI_SOURCE : OCI_REVISION} label`,
    };
  }
  if (source.replace(/\/$/, "").toLowerCase() !== deployed.toLowerCase()) {
    return {
      how: `${step}, but the OCI config of ${image} labels ${OCI_SOURCE} ${source}, not the deployment's ${deployed}`,
    };
  }
  const labels = `the OCI config of ${image} labels ${OCI_SOURCE} ${source} and ${OCI_REVISION} ${revision}`;
  if (!/^[0-9a-f]{40}$/.test(revision)) {
    return { how: `${step}, but ${labels}, which names no commit (40 hex digits)` };
  }
  const asked = commitsRead(deployment, revision, when.at);
  let commit: string | undefined;
  try {
    commit = await github.commitAt(deployment, revision, when.at);
  } catch (error) {
    // A commit the deployment does not hold, or one made after the read.
    if (transientFailure(error)) throw error;
  }
  if (commit !== revision) {
    return {
      how: `${step}, but ${labels}, which is no commit of ${deployment} at or before ${when.words} ${when.at} (${asked})`,
    };
  }
  const labelled = `${step}: ${labels}, a commit of ${deployment} (${asked})`;
  const recipe = `${deployment}:${built.dockerfile} at ${revision} (gh api repos/${deployment}/contents/${built.dockerfile}?ref=${revision})`;
  const text = await github.text(deployment, revision, built.dockerfile);
  if (text === undefined) return { how: `${labelled}; ${recipe} does not exist` };
  const base = finalStage(text).base;
  const baseDigest = /@sha256:([0-9a-f]{64})$/.exec(base ?? "")?.[1];
  if (base === undefined || baseDigest === undefined) {
    return {
      how: `${labelled}; ${recipe} ${base === undefined ? "names no base of its final stage" : `builds its final stage on ${base}, which pins no digest`}`,
    };
  }
  const on = `${labelled}; ${recipe} builds its final stage on ${base}`;
  const baseConfig = await github.imageConfig(base);
  if (baseConfig === undefined) {
    return { how: `${on}, but its registry gives no OCI config of ${base}` };
  }
  // A build can pass the Dockerfile another base than its ARG default, so the images confirm it.
  const layers = builtOn(
    { name: base, diffIds: baseConfig.diffIds },
    { name: image, diffIds: config.diffIds }
  );
  if (!layers.on) {
    return {
      how: `${on}, but ${layers.words}, so ${image} is not built on the base the Dockerfile names`,
    };
  }
  const confirmed = `${on}; ${layers.words}, so the image is built on it`;
  const provenance = `gh api repos/${imageRepo}/attestations/sha256:${baseDigest}`;
  const found = await github.attestedCommit(imageRepo, baseDigest);
  if (found !== undefined) {
    return {
      sha: found.commit,
      how: `${confirmed}; ${provenance} names ${found.uri} at ${found.commit}`,
    };
  }
  const without = `the base's digest has no build provenance that names a commit (${provenance})`;
  const baseLabels = await labelCommit(github, { image: base, imageRepo, when });
  return baseLabels.sha === undefined
    ? { how: `${confirmed}, but ${without}, and ${baseLabels.how}` }
    : { sha: baseLabels.sha, how: `${confirmed}; ${without}; ${baseLabels.how}` };
}

/** The commit of `imageRepo` that the image the `key:` line names for the project `project` was
 *  built from, read from `deployment` at `sha` by LEGION_IMAGE_RULE: the project file's top-level
 *  `key:` line, else the one of the project's entry in the Pulumi config; `how` gives the reads
 *  that decided it, or, with no `sha`, why no source gave one: the build provenance of the image's
 *  digest, then the OCI config labels of the image itself (`labelCommit`), then, for an image of a
 *  repository `deploymentImages` names, the base the deployment built it on (`deploymentCommit`),
 *  else the comment lines above its line. Throws when a GitHub read fails other than with a 404 of
 *  the project file, of the config file, of a Dockerfile or of a provenance. */
async function imageCommit(
  github: GithubReads,
  read: {
    readonly key: ImageKey;
    readonly deployment: string;
    readonly sha: string;
    readonly projectPath: string;
    readonly project: string;
    readonly imageRepo: string;
    readonly when: { readonly at: string; readonly words: string };
    /** The configuration's deploymentImages. */
    readonly deploymentImages: readonly DeploymentImage[];
  }
): Promise<{ readonly sha?: string; readonly how: string }> {
  const { key, deployment, sha, project, imageRepo, when } = read;
  const file = path.posix.join(
    path.posix.dirname(path.posix.dirname(read.projectPath)),
    "Pulumi.prod.yaml"
  );
  const line = `${key}: line`;
  // The project file's own top-level line first (where the deployment keeps a project's values
  // since they moved out of the Pulumi config), else the Pulumi config's - key: <project> entry.
  const own = `${deployment}:${read.projectPath} at ${sha}`;
  const ownText = await github.text(deployment, sha, read.projectPath);
  let entry = ownText === undefined ? undefined : projectFileImage(ownText, key);
  const config = `${deployment}:${file} at ${sha}`;
  let named = `${own} gives the project ${project} the ${key} ${entry?.image}`;
  if (entry === undefined) {
    const none = `${own} has no top-level ${line}`;
    const text = await github.text(deployment, sha, file);
    if (text === undefined) {
      return {
        how: `${none}, and ${config} does not exist (gh api repos/${deployment}/contents/${file}?ref=${sha})`,
      };
    }
    entry = projectImage(text, project, key);
    if (entry === undefined) {
      return {
        how: `${none}, and ${config} holds no entry - key: ${project} with ${key === "image" ? "an" : "a"} ${line}`,
      };
    }
    named = `${config} gives the project ${project} the ${key} ${entry.image}`;
  }
  const digest = /@sha256:([0-9a-f]{64})\b/.exec(entry.image)?.[1];
  let without = ", which names no digest";
  if (digest !== undefined) {
    const found = await github.attestedCommit(imageRepo, digest);
    const asked = `gh api repos/${imageRepo}/attestations/sha256:${digest}`;
    if (found !== undefined) {
      return { sha: found.commit, how: `${named}; ${asked} names ${found.uri} at ${found.commit}` };
    }
    without = `, whose digest has no build provenance that names a commit (${asked})`;
  }
  // The image itself: the labels of its OCI config, read from the registry by digest, name the
  // commit it was built from, or the release it is.
  const labelled = await labelCommit(github, { image: entry.image, imageRepo, when });
  if (labelled.sha !== undefined) {
    return { sha: labelled.sha, how: `${named}${without}; ${labelled.how}` };
  }
  if (labelled.how !== "") without = `${without}, and ${labelled.how}`;
  // An image the deployment built on a worker image: the commit of the base its Dockerfile names,
  // once the images confirm that base; a step that fails leaves the commit not known.
  const repository = entry.image.replace(/@.*$/, "").replace(/:[^/]*$/, "");
  const built = read.deploymentImages.find((each) => each.repository === repository);
  if (built !== undefined) {
    const based = await deploymentCommit(github, {
      image: entry.image,
      built,
      deployment,
      imageRepo,
      when,
    });
    const how = `${named}${without}; ${based.how}`;
    return based.sha === undefined ? { how } : { sha: based.sha, how };
  }
  if (entry.comment.length === 0) {
    return { how: `${named}${without}, and no comment line is right above its ${line}` };
  }
  const comment = entry.comment.join(" ");
  const above = `${named}${without}; the comment right above its ${line}`;
  // The comment's words in their order: an image digest, a tag (`<name>-v<version>`, `v<version>`)
  // or a commit (7 to 40 hex digits, letters and digits both: neither a run number nor a word).
  const words = comment.matchAll(
    /@sha256:([0-9a-f]{64})\b|((?:[\w.-]+-)?v\d+(?:\.\d+)+)|\b([0-9a-f]{7,40})\b/g
  );
  const tried = new Set<string>();
  for (const [word, base, tag, hex] of words) {
    if (tried.has(word) || (base !== undefined && base === digest)) continue;
    tried.add(word);
    if (base !== undefined) {
      const found = await github.attestedCommit(imageRepo, base);
      if (found !== undefined) {
        return {
          sha: found.commit,
          how: `${above} names the image digest sha256:${base}, whose build provenance (gh api repos/${imageRepo}/attestations/sha256:${base}) names ${found.uri} at ${found.commit}`,
        };
      }
      continue;
    }
    const ref = tag ?? hex ?? "";
    if (tag === undefined && !(/[a-f]/.test(ref) && /[0-9]/.test(ref))) continue;
    if (tag !== undefined && !(await github.hasTag(imageRepo, tag))) continue;
    let commit: string;
    try {
      commit = await github.commitAt(imageRepo, ref, when.at);
    } catch (error) {
      // A word that is no commit of the repository, or a commit made after the read.
      if (transientFailure(error)) throw error;
      continue;
    }
    if (tag !== undefined) {
      return {
        sha: commit,
        how: `${above} names ${tag}, a tag of ${imageRepo} (gh api repos/${imageRepo}/git/matching-refs/tags/${tag}), whose last commit at or before ${when.words} ${when.at} is ${commit}`,
      };
    }
    if (commit.startsWith(ref)) {
      const asked = commitsRead(imageRepo, ref, when.at);
      return {
        sha: commit,
        how: `${above} names ${ref}, the commit ${commit} of ${imageRepo} (${asked})`,
      };
    }
  }
  return {
    how: `${above} names no image digest whose build provenance names a commit, no commit and no tag of ${imageRepo}`,
  };
}

/** A listing measure: `kind` says which listing the rebuilt one is set against (the session's own
 *  recorded prompt's, a subagent's of it, or none: `not measurable`). It is `session listing`
 *  exactly when the case's or task's profile holds the session's recorded system prompt
 *  (`ListingInput.recorded`, taken from the profile's `systemPrompt`), which capture.ts sends
 *  whole, so the kind also says what the capture shows the model: the recorded listing
 *  (`session listing`), whatever the profile installs, or a listing built from the profile
 *  (`subagent listing`); score.ts's `listingStanding` reads it so. A task that keeps an old
 *  profile is `not measurable` whatever that profile holds. `recorded` the names that
 *  listing holds, `listed` those the rebuilt profile lists (its skills less those Oh My Pi hides),
 *  `missing` the recorded names the profile does not list, `added` the names it lists that the
 *  recording lacks, `missingShare` missing ÷ recorded (null with no recorded name);
 *  `sessionDigest` the session's system-prompt digest (a case's last before its step, a task's
 *  first), `rebuiltDigest` always null (the build makes no prompt), `subagent` the subagent whose
 *  listing is used, and `why` in words. */
export const ListingMeasure = z.strictObject({
  kind: z.enum(["session listing", "subagent listing", "not measurable"]),
  recorded: z.number().nullable(),
  listed: z.number(),
  missing: z.array(z.string()),
  added: z.array(z.string()),
  missingShare: z.number().nullable(),
  sessionDigest: z.string().nullable(),
  rebuiltDigest: z.null(),
  subagent: z
    .strictObject({
      pathHash: z.string(),
      agent: z.string(),
      subagents: z.number(),
      recorded: z.number(),
    })
    .nullable(),
  why: z.string(),
  /** The listed names of each pool the profile installed because the recorded listing names a
   *  skill of it that no installed unit outside the pools ships (POOL_LISTING_RULE): the measure
   *  cannot see a miss of them. Absent where none is, and in a measure written before the mark. */
  fromRecorded: z.array(z.string()).optional(),
});
export type ListingMeasure = z.infer<typeof ListingMeasure>;

/** The times a top-level session's subagents spawned under the prompt build its profile is read
 *  at fall in (TOP_BUILD_RULE): at or after `from` and before `to`, each null for no bound, and in
 *  words (`words`); or, when no prompt build of the session is known, why (`unknown`). */
type BuildWindow =
  | { readonly from: string | null; readonly to: string | null; readonly words: string }
  | { readonly unknown: string };

/** What a listing measure of one session is made from: the names its rebuilt profile lists, its
 *  recorded system prompt when it has one (a subagent's), its system-prompt digest (a case's last
 *  before its step, a task's first), and, for a top-level session, the window of the build its
 *  profile is read at. */
interface ListingInput {
  readonly listed: readonly string[];
  readonly recorded: readonly string[] | undefined;
  readonly sessionDigest: string | null;
  readonly build?: BuildWindow;
  /** The names the profile took from the recorded listing (ProfileResult's `fromRecorded`). */
  readonly fromRecorded?: readonly string[];
}

/** What subagents.sql found of a top-level session's subagents between two times: the earliest
 *  whose recorded prompt holds a skill listing (`listing`: its path hash, agent, prompt's blocks
 *  and start), if one is, and how many subagents the session has, how many of them record a
 *  listing, and how many of those started between the times (`under`). */
interface SubagentsFound {
  readonly listing?: {
    readonly pathHash: string;
    readonly agent: string;
    readonly blocks: readonly string[];
    readonly start: string | null;
  };
  readonly subagents: number;
  readonly recorded: number;
  readonly under: number;
}

const SubagentRow = z.strictObject({
  parent: z.string(),
  from: z.string().nullable(),
  to: z.string().nullable(),
  hash: z.string().nullable(),
  start: z.string().nullable(),
  init: z.string().nullable(),
  subagents: z.number(),
  recorded: z.number(),
  under: z.number(),
});

/** The times of a subagents.sql query of one session: those of its build's window, else none. */
type Between = { readonly from: string | null; readonly to: string | null };

/** The key of a session's path hash and the times of its query in `subagentListings`'s map. */
const betweenKey = (hash: string, between: Between) =>
  `${hash}\0${between.from ?? ""}\0${between.to ?? ""}`;

/** What subagents.sql finds of each session of `sessions` (its path hash and the times its
 *  subagent's start must fall between) in the store `store` that has a subagent, by `betweenKey`:
 *  one query, none for no session. */
async function subagentListings(
  command: string,
  store: string,
  sessions: readonly ({ readonly hash: string } & Between)[]
): Promise<Map<string, SubagentsFound>> {
  const found = new Map<string, SubagentsFound>();
  if (sessions.length === 0) return found;
  const wanted = new Map(sessions.map((each) => [betweenKey(each.hash, each), each]));
  const query = subagentsSql([...wanted.values()]);
  for (const row of await storeRows(command, store, query)) {
    const parsed = SubagentRow.safeParse(row);
    if (!parsed.success) continue;
    const { parent, from, to, hash, start, init, subagents, recorded, under } = parsed.data;
    const session = init === null ? undefined : SessionInit.safeParse(parseJson(init));
    found.set(betweenKey(parent, { from, to }), {
      ...(hash === null || session?.success !== true
        ? {}
        : {
            listing: {
              pathHash: hash,
              agent: session.data.agent,
              blocks: session.data.systemPrompt,
              start,
            },
          }),
      subagents,
      recorded,
      under,
    });
  }
  return found;
}

/** What of a session's listing input says which listing its measure sets its profile against:
 *  its own recorded prompt, and the window of the build its profile is read at. */
type ListingBase = Pick<ListingInput, "recorded" | "build">;

/** The times a session's subagents.sql query takes from its listing input: its build's window, or
 *  undefined when the session needs no query (it recorded its own listing, or no build of it is
 *  known). */
function betweenOf(input: ListingBase): Between | undefined {
  if (input.recorded !== undefined || input.build === undefined || "unknown" in input.build) {
    return undefined;
  }
  return { from: input.build.from, to: input.build.to };
}

/** Why the store is not asked for a session's subagents when `betweenOf` gives no times. */
function unasked(input: ListingBase): SubagentRead {
  return {
    unread:
      input.recorded !== undefined
        ? "it recorded its own listing, so no subagent of it is asked for"
        : input.build !== undefined && "unknown" in input.build
          ? input.build.unknown
          : "no prompt build of it is known, so no subagent of it is asked for",
  };
}

/** What the store gives of the subagents of the session `pathHash` of `store`, whose listing input
 *  is `input`: one subagents.sql query for its build's times (`betweenOf`), sent through
 *  `readStore` where a build reads its stores one query at a time, before its profile is built,
 *  since the listing it finds decides the profile's pools (POOL_LISTING_RULE); or why it was not
 *  asked (`unasked`). Throws when the query fails. */
async function readSubagents(
  command: string,
  store: string,
  pathHash: string,
  input: ListingBase,
  readStore: <T>(read: () => Promise<T>) => Promise<T> = (read) => read()
): Promise<SubagentRead> {
  const between = betweenOf(input);
  if (between === undefined) return unasked(input);
  const found = await readStore(() =>
    subagentListings(command, store, [{ hash: pathHash, ...between }])
  );
  return { found: found.get(betweenKey(pathHash, between)) };
}

/** When a subagent's skills were loaded, and where Oh My Pi's source says so. */
const PARENT_LOAD_RULE =
  "Oh My Pi 18.18.6 gives a subagent the skills its parent loaded (task/structured-subagent.ts, lines 806-818 and 895-902), and a subagent of a subagent its parent's, so from its root session's last prompt build before its start (each build writes one system-prompt-digest line: session/session-tools.ts, lines 1795-1830); the subagent rescans only the skill directories an extension names (session/session-tools.ts, lines 2350-2560). So a subagent's profile reads each repository at the root's last prompt build before the subagent's start (digest-times.sql), but a pool's (a repository of an extension's skills-sources pool: copies.ts's pool units) at the subagent's start, and every repository at the subagent's start when the store holds no prompt build of its root before it";

/** When a session's repositories are read (PARENT_LOAD_RULE for a subagent, TOP_BUILD_RULE for a
 *  top-level session): at `at`, `words` saying what that time is, or every one at the session's
 *  start when `at` is undefined; a pool's at the session's start when `poolsAtStart` (a
 *  subagent's, which rescans its pools as it starts); and the `read times` evidence item that says
 *  which times and why. */
interface ReadTime {
  readonly at: string | undefined;
  /** The first digest's time, when the older repository rule uses session start instead. */
  readonly firstPromptAt?: string | undefined;
  readonly words: string;
  readonly poolsAtStart: boolean;
  readonly line: Evidence;
}

/** A system-prompt-digest line of a session's file: when Oh My Pi wrote it, and the digest. */
const DigestLine = z.strictObject({ at: z.string(), digest: z.string() });
type DigestLine = z.infer<typeof DigestLine>;

const DigestRow = z.strictObject({
  hash: z.string(),
  root: z.string(),
  rootHeld: z.boolean(),
  digests: z.array(DigestLine),
});
type DigestRow = z.infer<typeof DigestRow>;

/** The digest-times.sql row of each session of `pathHashes` the store `store` holds, by path
 *  hash: one query, none for no session. */
async function digestRows(
  command: string,
  store: string,
  pathHashes: readonly string[]
): Promise<Map<string, DigestRow>> {
  const rows = new Map<string, DigestRow>();
  if (pathHashes.length === 0) return rows;
  for (const row of await storeRows(command, store, digestTimesSql([...new Set(pathHashes)]))) {
    const parsed = DigestRow.safeParse(row);
    if (parsed.success) rows.set(parsed.data.hash, parsed.data);
  }
  return rows;
}

/** The ReadTime of a subagent session of the store `store` that started at `before`, from its
 *  digest-times.sql row `row` (undefined when the store holds no file of it now): its root's last
 *  prompt build before its start (PARENT_LOAD_RULE), else its start. */
function parentLoad(row: DigestRow | undefined, before: string, store: string): ReadTime {
  const builds = row?.digests.filter((each) => Date.parse(each.at) < Date.parse(before)) ?? [];
  const at = builds.at(-1)?.at;
  let why: string;
  if (row === undefined) {
    why = `the store ${store} holds no file of the subagent now`;
  } else if (!row.rootHeld) {
    why = `the store ${store} holds no file of its root (${row.root.slice(0, 12)})`;
  } else if (at === undefined) {
    why = `its root (${row.root.slice(0, 12)}) holds no system-prompt-digest line before it`;
  } else {
    why = `its root (${row.root.slice(0, 12)}) built its prompt last at ${at} before it (${builds.length} builds)`;
  }
  return {
    at,
    words: "the root's last prompt build",
    poolsAtStart: true,
    line: {
      item: "read times",
      source: `the subagent started at ${before}; ${why}, so ${
        at === undefined
          ? `each repository is read at the subagent's start ${before}`
          : `each repository is read at ${at}, and a pool's at the subagent's start ${before}`
      }: ${PARENT_LOAD_RULE}`,
    },
  };
}

/** When a top-level session's skills were loaded, and where Oh My Pi's source says so. */
const TOP_BUILD_RULE =
  "Oh My Pi 18.18.6 writes a system-prompt-digest line in a session's file when its prompt changes, just before the first reply under the new prompt (session/session-tools.ts, lines 1818-1827), and a top-level session reads every skill directory again, a pool's among them, as it starts and on each reload that rebuilds its prompt (session/session-tools.ts, lines 2321-2337, 2466-2506 and 2530-2547), each before the line of the build it makes. So a top-level session's profile reads each repository, a pool's too, at the line of the prompt build its step ran under: the last line at or before the step's message, or the line after that one when it holds the case's digest and that one does not (the step was the first reply under its build, whose line Oh My Pi writes as that reply ends, after the message's time); and at the session's start for its first build, which Oh My Pi makes as the session starts. A task, which runs as its session's first message, takes the session's first build; a context session takes the build its measured subagent was spawned under (the last line before that subagent's start)";

/** The digest lines of a transcript, of every branch, earliest first, as digest-times.sql gives a
 *  root's. */
function digestLines(t: Transcript): DigestLine[] {
  return t.entries
    .flatMap((entry) =>
      entry.type === "custom" &&
      entry.customType === "system-prompt-digest" &&
      typeof entry.data === "string" &&
      typeof entry.timestamp === "string" &&
      entryTime(entry) > 0
        ? [{ at: entry.timestamp, digest: entry.data }]
        : []
    )
    .sort(
      (a, b) =>
        Date.parse(a.at) - Date.parse(b.at) ||
        (a.digest < b.digest ? -1 : a.digest > b.digest ? 1 : 0)
    );
}

/** The time of a message as an ISO time, from its `timestamp` (epoch milliseconds, as Oh My Pi
 *  records a message's), or undefined. */
function messageTime(message: SessionMessage): string | undefined {
  const ms = message.timestamp;
  return typeof ms === "number" && Number.isFinite(ms) ? new Date(ms).toISOString() : undefined;
}

/** The index in `lines` (a top-level session's digest lines, earliest first) of the build a step
 *  whose message is at `stepAt` ran under, the case's digest being `digest` (TOP_BUILD_RULE): the
 *  last line at or before it (`last`), or the line after that one when it holds `digest` and that
 *  one does not; -1 for none. */
function stepLine(
  lines: readonly DigestLine[],
  stepAt: string,
  digest: string | null
): { readonly index: number; readonly last: number } {
  const step = Date.parse(stepAt);
  const last = lines.findLastIndex((line) => Date.parse(line.at) <= step);
  const own = last >= 0 && digest !== null && lines[last]?.digest !== digest;
  return { index: own && lines[last + 1]?.digest === digest ? last + 1 : last, last };
}

/** The ReadTime and BuildWindow of a top-level session that started at `start` and whose digest
 *  lines are `lines`, at the build `index` of them (TOP_BUILD_RULE): the build of that line, which
 *  `words` names, or the first build for an index below 1; `how` says how the build was found.
 *  With `lines` a text, no build is known, and that text says why: every repository is read at the
 *  session's start. */
function topBuild(
  lines: readonly DigestLine[] | string,
  start: string,
  index: number,
  how: string,
  words = "the prompt build its step ran under"
): { readonly read: ReadTime; readonly window: BuildWindow } {
  const first = (source: string): ReadTime => ({
    at: undefined,
    words: "the session's start",
    poolsAtStart: false,
    line: { item: "read times", source: `${source}: ${TOP_BUILD_RULE}` },
  });
  if (typeof lines === "string") {
    return {
      read: first(
        `${lines}, so no prompt build of it is known, and each repository is read at the session's start ${start}`
      ),
      window: { unknown: `no prompt build of the session is known (${lines})` },
    };
  }
  const line = lines[index];
  const next = lines[index + 1];
  const count = `the session's ${lines.length} system-prompt-digest line${lines.length === 1 ? "" : "s"}`;
  if (index < 1 || line === undefined) {
    const second = lines[1];
    return {
      read: {
        ...first(
          `${how}; so it is the session's first build (${
            lines[0] === undefined
              ? "the session holds no system-prompt-digest line"
              : `its line is the first of ${count}, at ${lines[0].at}, digest ${lines[0].digest}`
          }), and each repository, a pool's too, is read at the session's start ${start}`
        ),
        firstPromptAt: lines[0]?.at,
      },
      window: {
        from: null,
        to: second?.at ?? null,
        words: `under the session's first prompt build, the one its profile is read at (${
          second === undefined ? "no later line" : `before the second line, at ${second.at}`
        })`,
      },
    };
  }
  return {
    read: {
      at: line.at,
      words,
      poolsAtStart: false,
      line: {
        item: "read times",
        source: `${how}; so it is the build of line ${index + 1} of ${count}, at ${line.at}, digest ${line.digest}, and each repository, a pool's too, is read at ${line.at}: ${TOP_BUILD_RULE}`,
      },
    },
    window: {
      from: line.at,
      to: next?.at ?? null,
      words: `under the prompt build its profile is read at (from its line, at ${line.at}, ${
        next === undefined ? "with no later line" : `to the next line, at ${next.at}`
      })`,
    },
  };
}

/** The build of a top-level case's step (TOP_BUILD_RULE) from its session's digest lines `lines`
 *  (a text when none are known: why), its start, its step's message time `stepAt` (undefined when
 *  the case records none) and its digest. */
function stepBuild(
  lines: readonly DigestLine[] | string,
  start: string,
  stepAt: string | undefined,
  digest: string | null
): { readonly read: ReadTime; readonly window: BuildWindow } {
  if (typeof lines === "string" || stepAt === undefined) {
    return topBuild(
      lines,
      start,
      -1,
      "the case records no time of its step's message, so its step is taken to run under the session's first build"
    );
  }
  const { index, last } = stepLine(lines, stepAt, digest);
  const how =
    index < 0
      ? `the step's message is at ${stepAt}, before every system-prompt-digest line of the session`
      : index === last
        ? `the step's message is at ${stepAt}, and the last system-prompt-digest line at or before it is at ${lines[index]?.at}`
        : `the step's message is at ${stepAt}; the last system-prompt-digest line at or before it, at ${lines[last]?.at}, holds ${lines[last]?.digest}, not the case's digest ${digest}, and the next, at ${lines[index]?.at}, holds it: the step was the first reply under that build`;
  return topBuild(lines, start, index, how);
}

/** The lines of the `<skills>` listing of the recorded prompt `blocks` (`- <name>: <text>`), read
 *  by capture.ts's `listingOf`, the parser its `skillListing` reads a body's with: each name with
 *  the text it is listed under, the first line of each name, in order. */
function listingLines(blocks: readonly string[]): { name: string; text: string }[] {
  return [...listingOf(blocks)].map(([name, text]) => ({ name, text }));
}

/** The names of the `<skills>` listing of the recorded prompt `blocks`, in order
 *  (`listingLines`). */
function listingNames(blocks: readonly string[]): string[] {
  return listingLines(blocks).map((line) => line.name);
}

/** Where Oh My Pi says a subagent's listing is its parent's, and what the measure compares. */
const SUBAGENT_LISTING_RULE =
  "Oh My Pi 18.18.6 gives a subagent its parent's skills unfiltered (task/structured-subagent.ts, resolveAutoloadSkills, lines 811-818, and line 900; task/executor.ts, lines 662-666 and 5256)";
const NAMES_ONLY =
  "names are compared, not descriptions: Oh My Pi lists under each name a model's hint for the skill or a preview of its description (system-prompt.ts, lines 981-983; extensibility/skill-descriptions.ts, lines 448-476)";

/** What the store gave of a session's subagents: what one subagents.sql query found (`found`,
 *  undefined when the session has no subagent), or why no query answered (`unread`: the session
 *  needs none, is gone, or the query failed). The measure never says what the store holds unless
 *  it was asked. */
type SubagentRead = { readonly found: SubagentsFound | undefined } | { readonly unread: string };

/** The recorded listing a session's measure sets its profile against (`listingMeasure`): its own
 *  recorded prompt's blocks, else those of the subagent `read` found, the earliest spawned under
 *  the build its profile is read at (`subagent`); undefined where neither is, and where that
 *  build is not known. */
function measuredAgainst(
  input: ListingBase,
  read: SubagentRead
):
  | { readonly blocks: readonly string[]; readonly subagent?: SubagentsFound["listing"] }
  | undefined {
  if (input.recorded !== undefined) return { blocks: input.recorded };
  if (input.build !== undefined && "unknown" in input.build) return undefined;
  const subagent = "found" in read ? read.found?.listing : undefined;
  return subagent === undefined ? undefined : { blocks: subagent.blocks, subagent };
}

/** The recorded listing that decides a profile's pools (POOL_LISTING_RULE): the one its listing
 *  measure sets it against (`measuredAgainst`), the session's own or its measured subagent's, by
 *  its names (`listingNames`) and where it was recorded; undefined where there is none. */
function decidingListing(input: ListingBase, read: SubagentRead): DecidingListing | undefined {
  const against = measuredAgainst(input, read);
  if (against === undefined) return undefined;
  return {
    names: listingNames(against.blocks),
    from:
      against.subagent === undefined
        ? "the session's recorded listing"
        : `the recorded listing of its subagent ${against.subagent.pathHash}, the one its listing measure uses,`,
  };
}

/** The listing measure of one session (score.ts's LISTING_MEASURE_ITEM): against its own recorded
 *  listing, else against the listing of the subagent `read` found, the earliest spawned under the
 *  build its profile is read at (`input.build`), else not measurable, with why. */
function listingMeasure(input: ListingInput, read: SubagentRead): Evidence {
  const listed = [...new Set(input.listed)].sort();
  const build = input.build;
  const unknown = build !== undefined && "unknown" in build ? build.unknown : undefined;
  const words = build === undefined || "unknown" in build ? "at any time" : build.words;
  const found = "found" in read ? read.found : undefined;
  const against = measuredAgainst(input, read);
  const subagent = against?.subagent;
  const digest = `the session's system-prompt-digest (a case's last before its step, a task's first) is ${input.sessionDigest ?? "none"}, and the rebuilt prompt has no digest: the build makes no prompt`;
  let measure: ListingMeasure;
  if (against === undefined) {
    let none: string;
    if (unknown !== undefined) {
      none = `${unknown}, so no subagent of it can be tied to the build its profile is read at`;
    } else if ("unread" in read) {
      none = read.unread;
    } else if (found === undefined) {
      none = `the store holds no subagent of it (subagents.sql, asked for those spawned ${words})`;
    } else {
      none = `the store holds no subagent of it whose recorded prompt holds a skill listing spawned ${words} (subagents.sql: ${found.recorded} of its ${found.subagents} subagents record one, ${found.under} of them spawned then), and the listing of a subagent spawned under another build is never used`;
    }
    measure = {
      kind: "not measurable",
      recorded: null,
      listed: listed.length,
      missing: [],
      added: [],
      missingShare: null,
      sessionDigest: input.sessionDigest,
      rebuiltDigest: null,
      subagent: null,
      why: `the session records no system prompt, only its digest; ${digest}; ${none}`,
    };
  } else {
    const recorded = listingNames(against.blocks);
    const missing = recorded.filter((name) => !listed.includes(name));
    // `measuredAgainst` gives a subagent only where the session recorded no prompt of its own.
    const fromSubagent = subagent !== undefined && found !== undefined;
    const from = fromSubagent
      ? `the session records only its prompt's digest, and ${digest}; the listing is the recorded one of its subagent ${subagent.pathHash} (agent ${subagent.agent}, started at ${subagent.start ?? "no recorded time"}), the earliest of the ${found.under} of its ${found.subagents} subagents whose recorded prompt holds a listing (${found.recorded} in all) spawned ${words}, since ${SUBAGENT_LISTING_RULE}`
      : "the session recorded its system prompt, and its capture sends it whole";
    measure = {
      kind: input.recorded !== undefined ? "session listing" : "subagent listing",
      recorded: recorded.length,
      listed: listed.length,
      missing,
      added: listed.filter((name) => !recorded.includes(name)),
      missingShare: recorded.length === 0 ? null : missing.length / recorded.length,
      sessionDigest: input.sessionDigest,
      rebuiltDigest: null,
      subagent: fromSubagent
        ? {
            pathHash: subagent.pathHash,
            agent: subagent.agent,
            subagents: found.subagents,
            recorded: found.recorded,
          }
        : null,
      why: `${from}; ${NAMES_ONLY}`,
    };
  }
  const taken = input.fromRecorded ?? [];
  return {
    item: LISTING_MEASURE_ITEM,
    source: JSON.stringify(taken.length === 0 ? measure : { ...measure, fromRecorded: taken }),
  };
}

/** `built` with its profile keeping the listing its measure sets it against (`measuredAgainst`;
 *  load.ts's RecordedListing), where it has one: where that listing was recorded, each of its
 *  lines with its secrets taken out, and the profile's copy of the tested skill's source. The
 *  lines come from the profile's own recorded prompt where the listing is the session's, whose
 *  secrets `sessionProfile` took out and counted already (`scrubbed({ systemPrompt })`), so none
 *  is taken out again; else from the blocks as the store gave them (a subagent's), whose secrets
 *  are taken out here and counted in `secretsRemoved`. This is the profile a task's file holds. */
function withRecordedListing(built: SessionProfile, read: SubagentRead): SessionProfile {
  const against = measuredAgainst(built.listing, read);
  if (against === undefined) return built;
  const { subagent } = against;
  const secrets = scrubbed(
    subagent === undefined ? (built.profile.systemPrompt ?? against.blocks) : against.blocks
  );
  const recordedListing: RecordedListing = {
    source:
      subagent === undefined
        ? { kind: "session" }
        : { kind: "subagent", pathHash: subagent.pathHash },
    lines: listingLines(secrets.value),
    ...(built.testedCopy === undefined ? {} : { tested: built.testedCopy }),
  };
  return {
    ...built,
    profile: { ...built.profile, recordedListing },
    secretsRemoved: built.secretsRemoved + secrets.removed,
  };
}

/** The names `evidence`'s listing measure says the profile took from the recorded listing
 *  (POOL_LISTING_RULE); none where it holds no measure or no mark. */
export function fromRecordedIn(
  evidence: readonly { readonly item: string; readonly source: string }[] | undefined
): readonly string[] {
  return measureIn(evidence)?.fromRecorded ?? [];
}

/** The listing measure that `evidence` holds, or undefined. */
function measureIn(evidence: readonly Evidence[] | undefined): ListingMeasure | undefined {
  const item = evidence?.find((each) => each.item === LISTING_MEASURE_ITEM);
  const parsed = item === undefined ? undefined : ListingMeasure.safeParse(parseJson(item.source));
  return parsed?.success === true ? parsed.data : undefined;
}

/** A summary's counts of listing measures: how many of each kind, and the missing and added names
 *  summed; `none` those that hold no measure. */
function listingCounts(measures: readonly (ListingMeasure | undefined)[]): Record<string, number> {
  const counts: Record<string, number> = {
    "session listing": 0,
    "subagent listing": 0,
    "not measurable": 0,
    none: 0,
    missing: 0,
    added: 0,
    fromRecorded: 0,
  };
  for (const measure of measures) {
    const kind = measure?.kind ?? "none";
    counts[kind] = (counts[kind] ?? 0) + 1;
    counts.missing = (counts.missing ?? 0) + (measure?.missing.length ?? 0);
    counts.added = (counts.added ?? 0) + (measure?.added.length ?? 0);
    if ((measure?.fromRecorded?.length ?? 0) > 0)
      counts.fromRecorded = (counts.fromRecorded ?? 0) + 1;
  }
  return counts;
}

/** `evidence` with `measure` before its audit item, or last when it has none. */
function withMeasure(evidence: readonly Evidence[], measure: Evidence): Evidence[] {
  const out = evidence.filter((each) => each.item !== LISTING_MEASURE_ITEM);
  const audit = out.findIndex((each) => each.item.startsWith(AUDIT_EVIDENCE));
  out.splice(audit < 0 ? out.length : audit, 0, measure);
  return out;
}

/** The names a rebuilt profile lists: its skills, a namespaced one as `<namespace>/<name>`, less
 *  those Oh My Pi hides (HIDDEN_RULE). */
function listedNames(result: Pick<ProfileResult, "profile" | "hidden">): string[] {
  return result.profile.skills
    .map((skill) => listedName(skill))
    .filter((name) => !result.hidden.has(name));
}

/** How Oh My Pi 18.18.6 lists two skills of one name. */
const NAMESPACE_RULE =
  "Oh My Pi 18.18.6 keeps the bare name for the skill of a name it admits first (a skills directory an extension names before any provider's skill) and lists a later one whose text (body or frontmatter) differs as <namespace>/<name>, the namespace its plugin's name, else the directory that holds its skills/, else its provider's id for a dotted directory, made one token of letters, digits, _ and - (extensibility/skills.ts, skillNamespace, lines 96-116, and resolveCollision, lines 164-223); a later one of the same text it drops";

/** The name or directory a copy of the unit `unit` takes its namespace from (`unitNamespace`),
 *  before Oh My Pi makes it one token; undefined for a unit it gives none. */
function namespaceRoot(unit: string): string | undefined {
  if (unit.startsWith("package:")) return unit.slice("package:".length).split("/").pop();
  if (unit.startsWith("plugin:")) return unit.slice("plugin:".length).split("@")[0];
  if (unit.startsWith("image:")) return unit.slice("image:".length);
  if (unit.startsWith("pool:")) return path.posix.basename(unit.slice("pool:".length));
  if (unit === "user-skills:omp") return "agent";
  if (unit === "user-skills:claude-code") return "claude";
  return undefined;
}

/** The namespace Oh My Pi gives a copy of the unit `unit` (NAMESPACE_RULE): the plugin's name a
 *  package (less its scope), a plugin or a worker image plugin is loaded under; else the directory
 *  that holds the copy's `skills/` where the host installs it: a pool's directory, `agent` for
 *  ~/.omp/agent/skills, and for ~/.claude/skills, a dotted directory, the Claude Code provider's id
 *  `claude` (discovery/claude.ts, line 43). Each is made one token as Oh My Pi makes it. Undefined
 *  for any other unit, and for a pool in a dotted directory, whose second copy a profile leaves
 *  out. */
function unitNamespace(unit: string): string | undefined {
  const root = namespaceRoot(unit);
  if (root === undefined || root.startsWith(".")) return undefined;
  const token = root.replace(/[^\p{L}\p{N}_-]+/gu, "-").replace(/^-+|-+$/g, "");
  return token === "" ? undefined : token;
}

/**
 * The session repository of a session of the context `context` whose working directory is `cwd`:
 * for a Legion context, the repository of its workspace (`/legion/workspaces/<owner>/<name>/
 * <workspace>/…`, the checkout at `<workspace>`); for any other (a dev box), DEVBOX_CHECKOUT_RULE:
 * the dev box's own repository `home` where `cwd` is in its checkout (`home.dir`), else the
 * repository of `repositories` (the copies output's list) named as the last part of `cwd`, else
 * of its nearest parent named as any of them, when one alone is named so; a parent's is marked
 * (`byParent`), and `caseProfile` takes it only where its tree holds the path below that parent,
 * or, for a path of one directory, where a file the session read under `cwd` is a file of the
 * tree at its path there: then `cwd` is a workspace of the repository, at its root. Otherwise
 * none, and the evidence says which repositories matched.
 */
function sessionRepository(
  context: string,
  cwd: string | undefined,
  repositories: Iterable<string>,
  home?: { readonly repo: string; readonly dir: string }
): Checkout {
  const none = (source: string): Checkout => ({
    repo: undefined,
    rel: "",
    line: { item: NO_SESSION_REPOSITORY_ITEM, source },
  });
  const inside = (rel: string) =>
    rel === "" ? "" : `; the working directory is ${rel} in the checkout`;
  if (context.startsWith("legion:")) {
    const workspace = LEGION_WORKSPACE.exec(cwd ?? "");
    if (cwd === undefined || workspace === null) {
      return none(
        `the session's working directory ${cwd ?? "(none)"} is no Legion workspace /legion/workspaces/<owner>/<name>/`
      );
    }
    const repo = `${workspace[1]}/${workspace[2]}`;
    const rel = cwd
      .slice(workspace[0].length)
      .split("/")
      .filter((part) => part !== "")
      .slice(1)
      .join("/");
    return {
      repo,
      rel,
      line: {
        item: `${SESSION_REPOSITORY_ITEM}${repo}`,
        source: `the session's working directory ${cwd} is a Legion workspace of it: /legion/workspaces/<owner>/<name>/${inside(rel)}`,
      },
    };
  }
  if (cwd === undefined) {
    return none(`${DEVBOX_CHECKOUT_RULE}. The session names no working directory`);
  }
  if (home !== undefined && (cwd === home.dir || cwd.startsWith(`${home.dir}/`))) {
    const rel = path.posix.relative(home.dir, cwd);
    return {
      repo: home.repo,
      rel,
      line: {
        item: `${SESSION_REPOSITORY_ITEM}${home.repo}`,
        source: `${DEVBOX_CHECKOUT_RULE}. The working directory ${cwd} is ${rel === "" ? "" : "under "}${home.dir}, the checkout of the dev box's repository ${home.repo}, whose .claude/ the configuration's devboxClaudeConfigDir names${inside(rel)}`,
      },
    };
  }
  const listed = [...repositories];
  for (let dir = cwd; ; ) {
    const name = path.posix.basename(dir);
    const named = listed.filter((repo) => repo.split("/")[1] === name).sort();
    const where = `The working directory ${cwd} is ${dir === cwd ? "" : `under ${dir}, `}named ${name}`;
    const [repo] = named;
    if (named.length === 1 && repo !== undefined) {
      const rel = path.posix.relative(dir, cwd);
      return {
        repo,
        rel,
        line: {
          item: `${SESSION_REPOSITORY_ITEM}${repo}`,
          source: `${DEVBOX_CHECKOUT_RULE}. ${where}, which ${repo} alone of the list is named${dir === cwd ? "" : `, and its tree holds ${rel}`}${inside(rel)}`,
        },
        ...(dir === cwd ? {} : { byParent: dir }),
      };
    }
    if (named.length > 1) {
      return none(
        `${DEVBOX_CHECKOUT_RULE}. ${where}, which ${named.length} repositories of the list are named (${named.join(", ")}), so which one it is a checkout of is not known`
      );
    }
    const parent = path.posix.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return none(
    `${DEVBOX_CHECKOUT_RULE}. No part of the working directory ${cwd} is named as a repository of the list`
  );
}

/** The skill directories of the session repository Oh My Pi 18.18.6 lists for a working directory
 *  in a checkout of it (its project skills), by provider in the order it admits them: each kind's
 *  directories (`dirs`) under the working directory alone (`at: "cwd"`) or under it and each of
 *  its parents up to the checkout's root, the nearest first (`at: "each"`; at each level the
 *  kind's `dirs` in order), each with its rank among the skills of one name (`unitRank`), whether
 *  Oh My Pi lists a skill of it without a description (`describedOnly`), where it reads it, and
 *  the namespace it lists a second copy of a name under (NAMESPACE_RULE): each directory is
 *  dotted, so its provider's id (extensibility/skills.ts, skillNamespace, lines 96-116). Every
 *  project provider is on: extensibility/skills.ts, isSourceEnabled, lines 341-363, turns off a
 *  user-level source alone. Providers of one priority are taken in the order discovery/index.ts
 *  imports them (lines 23-43; capability/index.ts, lines 177-184, puts a provider after those of
 *  its own priority). Each scan takes a directory entry or a symbolic link of each skill
 *  (discovery/helpers.ts, scanSkillsFromDir, lines 579-580 and 594-596), so a link a skills
 *  directory holds is followed. */
export const PROJECT_SKILLS = [
  {
    dirs: [".omp/skills"],
    at: "each",
    rank: 1,
    describedOnly: true,
    how: "Oh My Pi lists each <name>/SKILL.md that has a description under .omp/skills/ of the working directory and of each parent up to the repository's root, the nearest first (discovery/builtin.ts, loadSkills, lines 314-327, over getProjectAncestorDirs, lines 81-109)",
    namespace: "native",
  },
  {
    dirs: [".claude/skills"],
    at: "each",
    rank: 5,
    describedOnly: false,
    how: "Oh My Pi lists each <name>/SKILL.md under .claude/skills/ of the working directory and of each parent up to the repository's root, the nearest first (discovery/claude.ts, loadSkills, lines 205-224)",
    namespace: "claude",
  },
  {
    dirs: [".agent/skills", ".agents/skills"],
    at: "each",
    rank: 7,
    describedOnly: false,
    how: "Oh My Pi lists each <name>/SKILL.md under .agent/skills/ and .agents/skills/ of the working directory and of each parent up to the repository's root, the nearest first, .agent/ before .agents/ at each (discovery/agents.ts, loadSkills, lines 158-172, over getProjectPathCandidates, lines 140-155; priority 70, line 29)",
    namespace: "agents",
  },
  {
    dirs: [".codex/skills"],
    at: "cwd",
    rank: 8,
    describedOnly: false,
    how: "Oh My Pi lists each <name>/SKILL.md under .codex/skills/ of the working directory (discovery/codex.ts, loadSkills, lines 266-292, over getProjectCodexDir, lines 50-52; priority 70, line 48)",
    namespace: "codex",
  },
  {
    dirs: [".opencode/skills"],
    at: "cwd",
    rank: 9,
    describedOnly: false,
    how: "Oh My Pi lists each <name>/SKILL.md under .opencode/skills/ of the working directory (discovery/opencode.ts, loadSkills, lines 353-380, over discovery/helpers.ts's getProjectPath, lines 130-135; priority 55, line 50)",
    namespace: "opencode",
  },
  {
    dirs: [".github/skills"],
    at: "cwd",
    rank: 10,
    describedOnly: true,
    how: "Oh My Pi lists each <name>/SKILL.md that has a description under .github/skills/ of the working directory (discovery/github.ts, loadSkills, lines 286-296, over discovery/helpers.ts's getProjectPath, lines 130-135; priority 30, line 42)",
    namespace: "github",
  },
] as const;

/** Which of a project skill and another skill of its name Oh My Pi 18.18.6 gives a session, and
 *  where its source says so. */
const PROJECT_PRECEDENCE =
  "Oh My Pi 18.18.6 keeps under a name the first skill it admits, in its providers' order, highest priority first (capability/index.ts, lines 177-184 and 285-333; extensibility/skills.ts, resolveCollision, lines 141-223, lists a later one of other text under a namespace, as NAMESPACE_RULE says): its own provider (100) reads the project's .omp/skills of the working directory and of each parent up to the repository's root, the nearest first, before ~/.omp/agent/skills (discovery/builtin.ts, lines 81-109 and 314-342), extension packages come next (90, discovery/omp-plugins.ts, lines 136-169), then the Claude Code provider (80) reads ~/.claude/skills before the .claude/skills of the working directory and of each parent up to the repository's root, the nearest first (discovery/claude.ts, lines 205-249), then Claude Code's plugins (75 and 70, discovery/agent-plugins.ts and discovery/claude-plugins.ts), then the .agent/skills and .agents/skills of the working directory and of each parent (70, discovery/agents.ts, lines 140-172), the working directory's .codex/skills (70, discovery/codex.ts, lines 266-292), its .opencode/skills (55, discovery/opencode.ts, lines 353-380) and its .github/skills (30, discovery/github.ts, lines 286-296), the providers of one priority in the order discovery/index.ts imports them (lines 23-43); a skill of an extension's own directory (resources_discover) outranks them all (extensibility/skills.ts, lines 147-151)";

/** Where Oh My Pi 18.18.6 admits the skills of the unit `unit` among the skills of one name,
 *  earliest first (PROJECT_PRECEDENCE), counted as PROJECT_SKILLS counts a project's: a pool an
 *  extension's `resources_discover` names 0, ~/.omp/agent/skills 2, an extension package 3,
 *  ~/.claude/skills 4, a Claude Code plugin 6; undefined for any other unit. */
function unitRank(unit: string): number | undefined {
  if (unit.startsWith("pool:")) return 0;
  if (unit === "user-skills:omp") return 2;
  if (unit.startsWith("package:")) return 3;
  if (unit === "user-skills:claude-code") return 4;
  if (unit.startsWith("plugin:")) return 6;
  return undefined;
}

/** The project context files Oh My Pi 18.18.6 reads in a checkout, in its providers' order,
 *  highest priority first, each with its provider's priority, the levels it is read at (`each`:
 *  the working directory and each parent up to the checkout's root; `cwd`: the working directory
 *  alone; `omp`: the nearest level whose `.omp/` holds anything), whether an empty file takes its
 *  level's slot, and where the source says so. */
const CONTEXT_FILES: readonly {
  readonly file: string;
  readonly priority: number;
  readonly at: "each" | "cwd" | "omp";
  readonly empty: boolean;
  readonly source: string;
}[] = [
  {
    file: ".omp/AGENTS.md",
    priority: 100,
    at: "omp",
    empty: false,
    source:
      "discovery/builtin.ts, line 46 and lines 111-123 and 946-978: at the nearest level whose .omp/ holds anything",
  },
  {
    file: ".claude/CLAUDE.md",
    priority: 80,
    at: "cwd",
    empty: true,
    source:
      "discovery/claude.ts, line 45 and lines 72-77 and 180-192: at the working directory alone, an empty file too",
  },
  {
    file: ".agent/AGENTS.md",
    priority: 70,
    at: "each",
    empty: false,
    source: "discovery/agents.ts, line 29 and lines 140-155 and 275-291",
  },
  {
    file: ".agents/AGENTS.md",
    priority: 70,
    at: "each",
    empty: false,
    source: "discovery/agents.ts, line 29 and lines 140-155 and 275-291, after .agent/",
  },
  {
    file: ".gemini/GEMINI.md",
    priority: 60,
    at: "cwd",
    empty: false,
    source:
      "discovery/gemini.ts, line 42 and lines 148-164, over discovery/helpers.ts's getProjectPath (lines 130-135): at the working directory alone",
  },
  {
    file: ".github/copilot-instructions.md",
    priority: 30,
    at: "cwd",
    empty: false,
    source:
      "discovery/github.ts, line 42 and lines 48-66, over discovery/helpers.ts's getProjectPath (lines 130-135): at the working directory alone",
  },
  {
    file: "AGENTS.md",
    priority: 10,
    at: "each",
    empty: false,
    source:
      "discovery/agents-md.ts, lines 20-30, over discovery/helpers.ts's loadStandaloneContextFiles (lines 844-900)",
  },
  {
    file: "CLAUDE.md",
    priority: 10,
    at: "each",
    empty: false,
    source:
      "discovery/claude-md.ts, lines 20-30, over discovery/helpers.ts's loadStandaloneContextFiles (lines 844-900), registered after agents-md.ts (discovery/index.ts, lines 24-25)",
  },
];
/** How Oh My Pi 18.18.6 picks and orders the project context files a session gets. */
const CONTEXT_SLOT_RULE =
  "Oh My Pi 18.18.6 keeps one project context file at each level from the working directory up to the checkout's root (capability/context-file.ts, lines 36-46: one for each depth), the first in its providers' order, highest priority first (capability/index.ts, lines 177-184 and 285-333), and joins the files it keeps the farthest level first (system-prompt.ts, lines 392-398)";
/** The directories of each level that the session repository's read reads (GithubReads'
 *  `project`): each one a file of CONTEXT_FILES or a skills directory of PROJECT_SKILLS is in
 *  (`.omp`, `.claude`, `.agent`, `.agents`, `.gemini`, `.github`, `.codex`, `.opencode`). */
const PROJECT_LEVEL_DIRS = [
  ...new Set(
    [
      ...CONTEXT_FILES.map((spec) => spec.file),
      ...PROJECT_SKILLS.flatMap((kind) => kind.dirs),
    ].flatMap((file) => (file.includes("/") ? [file.slice(0, file.indexOf("/"))] : []))
  ),
];

/** What every profile of one build reads. */
type ProfileRun = Pick<CaseRun, "config" | "copies" | "github" | "placer">;

/** Whether the resolved installer row `row` (copies.ts's `resolveInstallers`) ships a copy as a
 *  member of its unit: it names the copy, by one of MEMBER_ACTIONS. */
function shipsMember(row: { readonly copyPath?: string | undefined; readonly action: string }) {
  return row.copyPath !== undefined && Object.hasOwn(MEMBER_ACTIONS, row.action);
}

/** A case's profile, the evidence for each part, and `shipped`: each copy some unit of the
 *  profile ships (`<repo>\0<name>`), whether or not a copy of its name came first. */
interface ProfileResult {
  readonly profile: Profile;
  readonly evidence: Evidence[];
  readonly shipped: ReadonlySet<string>;
  /** The names of the profile's skills Oh My Pi leaves out of its listing (HIDDEN_RULE). */
  readonly hidden: ReadonlySet<string>;
  /** Each other install path of a copy the profile holds, with the namespace Oh My Pi lists it
   *  under when its text there differs (NAMESPACE_RULE): the profile reads each repository at one
   *  commit, so it holds the copy once. */
  readonly again: readonly InstallPath[];
  /** The listed names of each pool the profile installs because the recorded listing names a skill
   *  of it that no installed unit outside the pools ships (POOL_LISTING_RULE), sorted; empty where
   *  no listing decided a pool. */
  readonly fromRecorded: readonly string[];
}

/** An install path of a copy: the copy, how its unit ships it, and its unit's namespace. */
interface InstallPath {
  readonly copy: { readonly repo: string; readonly path: string; readonly name: string };
  readonly how: string;
  readonly namespace: string;
}

/** Why a profile is refused: it installs no copy of the cut repository's skill at `when`. */
const notInstalled = (run: Pick<CaseRun, "skill" | "cut">, when = "the session's start") =>
  `the profile installs no ${run.skill} of ${run.cut.repo} at ${when}`;

const OMP_PROFILE_ITEM = "Oh My Pi profile";

/** The profile of a case, with the evidence for each part. Throws when a GitHub read fails. */
async function caseProfile(run: ProfileRun, input: ProfileInput): Promise<ProfileResult> {
  const { copies, github } = run;
  const evidence: Evidence[] = [];
  const commits = new Map<string, string>();
  const { ompProfile } = input;
  const legionStore = input.context.startsWith("legion:");
  // The Legion worker sets OMP_PROFILE=legion (packages/daemon/docker/worker.Dockerfile).
  const legion = legionStore && ompProfile === "legion";
  const named = ompProfile !== "default" && ompProfile !== "unknown";
  // Where the session's skills came from decides a named profile's context in a devbox store
  // (SESSION_CONTEXT_RULE): a `[Skill file: …]` header under the dev box's home shows a pod that
  // mounts that home, whose Oh My Pi loaded the dev box's skills, so the session is a dev-box
  // session for every step below. A read by path shows no skill Oh My Pi loaded.
  const headers = input.reads.filter((read) => read.byPath !== true);
  const homeDir = run.config.devboxHome;
  const mounted =
    input.context === "devbox" && named && homeDir !== undefined
      ? mountedHome(ompProfile, homeDir, headers)
      : undefined;
  const devbox = input.context === "devbox" && (ompProfile === "default" || mounted !== undefined);
  const isolated = !devbox && !legion;
  const inferReadUnits = !isolated || ompProfile === "unknown";
  // A header places its unit in every context, as direct proof; a read by path only where no
  // pod's links decide which units the pod ships.
  const placing = inferReadUnits ? input.reads : headers;
  let pod: PodImage | undefined;
  if (ompProfile !== "unknown") evidence.push({ item: OMP_PROFILE_ITEM, source: ompProfile });
  if (mounted !== undefined) {
    evidence.push(...mounted);
  } else if (input.context === "devbox" && named) {
    if (run.config.sessionPod === undefined)
      throw new PodNotMeasurable(`profile ${ompProfile} needs sessionPod in the configuration`);
    const at =
      input.readTime?.at ??
      input.readTime?.firstPromptAt ??
      (input.recordedPrompt ? undefined : input.sessionStart);
    if (at === undefined)
      throw new PodNotMeasurable(
        `the time of the root's prompt build for profile ${ompProfile} is unknown`
      );
    pod = await sessionPodImage(github, run.config.sessionPod, ompProfile, at);
    evidence.push({ item: `session pod profile ${ompProfile}`, source: pod.why });
  } else if (legionStore && ompProfile !== "unknown" && isolated) {
    evidence.push({
      item: `Legion pod profile ${ompProfile}`,
      source:
        "Inference: the worker image links plugins into its legion profile, not this profile. No explicit extension or image-installed user skill is known for this profile, so the rebuild includes its project skills.",
    });
  }
  // A dev box's own repository (DEVBOX_INSTALLERS_RULE) is the checkout whose .claude/ the
  // configuration names as its CLAUDE_CONFIG_DIR (DEVBOX_CHECKOUT_RULE); devboxHome's evidence is
  // pushed where it is below.
  const homeEvidence: Evidence[] = [];
  const home = devbox ? devboxHome(copies, homeEvidence) : undefined;
  const claudeDir = run.config.devboxClaudeConfigDir;
  let checkout =
    input.checkout ??
    sessionRepository(
      input.context,
      input.cwd,
      copies.branches.keys(),
      home === undefined || claudeDir === undefined
        ? undefined
        : { repo: home, dir: path.posix.dirname(claudeDir) }
    );
  evidence.push(checkout.line);
  // A checkout a parent's name gave is taken only where its repository's tree holds the working
  // directory's path below that parent, else, for a working directory directly inside that parent,
  // where a file the session read under it is a file of the tree: then it is a workspace of the
  // repository at its root (`byParent`, checked once the repository is read).
  let notCheckout = false;
  if (input.readTime !== undefined) evidence.push(input.readTime.line);
  // The time each repository is read at: a subagent's at its root's last prompt build before it
  // (PARENT_LOAD_RULE), but each repository a skills-sources pool clones at the subagent's start,
  // and a pool's host (the repository that holds its skills-sources.json, the plugin lists and
  // the installers) at the root's build too, and again at the subagent's start for its pool
  // directories (`poolAt`, below); a top-level session's every repository, a pool's too, at the
  // build its step ran under (TOP_BUILD_RULE).
  const poolRows = copies.installers.filter((row) => (row.unit ?? "").startsWith("pool:"));
  const poolHosts = new Set(
    poolRows.map((row) => (row.manifest_repository ?? "").toLowerCase()).filter((r) => r !== "")
  );
  const poolClones = new Set(
    poolRows
      .map((row) => (row.copy_repository ?? "").toLowerCase())
      .filter((repo) => repo !== "" && !poolHosts.has(repo))
  );
  const { readTime } = input;
  const whenOf = (repo: string): { at: string; words: string } =>
    readTime?.at !== undefined && !(readTime.poolsAtStart && poolClones.has(repo.toLowerCase()))
      ? { at: readTime.at, words: readTime.words }
      : { at: input.sessionStart, words: "the session's start" };
  // The project skill directories Oh My Pi reads, in its order: each kind of PROJECT_SKILLS, under
  // the working directory and then, for a kind read at each level, under each parent up to the
  // checkout's root (`levels`); at each level the kind's directories in order. A workspace found
  // once the session repository is read (below) is read at its root alone.
  const projectDirsAt = (at: readonly string[]) =>
    PROJECT_SKILLS.flatMap((kind) =>
      (kind.at === "cwd" ? at.slice(0, 1) : at).flatMap((level) =>
        kind.dirs.map((dir) => ({ ...kind, dir: level === "" ? dir : `${level}/${dir}` }))
      )
    );
  let levels = [checkout.rel];
  for (let level = checkout.rel; level !== ""; ) {
    level = dirOf(level);
    levels.push(level);
  }
  let projectDirs = projectDirsAt(levels);
  /** The commit of `repo` at the session's start, and the evidence for it; or no commit, when
   *  GitHub gives none then (NoCommitThen), and the evidence item that leaves it out, whose
   *  source names the read (the caller says what is not read). repositories.tsv names the ref the
   *  copies output read, a pin's tag for a pinned repository; a tag made after the session's start
   *  has no commit at or before it, and then the default branch's is read (an installer's pin at
   *  the session's start, below, takes its place where one names the repository). */
  const readCommit = async (repo: string): Promise<{ sha?: string; line: Evidence }> => {
    const when = whenOf(repo);
    let { branch, from } = await github.branch(repo);
    let younger = "";
    let sha: string | undefined;
    try {
      sha = await github.commitAt(repo, branch, when.at);
    } catch (error) {
      const fallBack = !transientFailure(error) && from === LISTED_BRANCH;
      if (!fallBack && !(error instanceof NoCommitThen)) throw error;
      if (fallBack) {
        younger = `${branch}, the ref repositories.tsv names, has no commit at or before ${when.words}; `;
        ({ branch, from } = await github.githubBranch(repo));
        sha = await github.commitAt(repo, branch, when.at).catch((again: unknown) => {
          if (again instanceof NoCommitThen) return undefined;
          throw again;
        });
      }
    }
    const read = commitsRead(repo, branch, when.at);
    if (sha === undefined) {
      return {
        line: {
          item: `left out: ${repo}, which has no commit at or before ${when.words} ${when.at}`,
          source: `${younger}${read} gives no commit of ${branch} (from ${from})`,
        },
      };
    }
    return {
      sha,
      line: {
        item: `commit ${sha} of ${repo}`,
        source: `${younger}the last commit of ${branch} (from ${from}) at or before ${when.words} ${when.at}: ${read}`,
      },
    };
  };
  const commitOf = async (repo: string): Promise<string> => {
    const known = commits.get(repo);
    if (known !== undefined) return known;
    const { sha, line } = await readCommit(repo);
    if (sha === undefined) throw new Error(line.source);
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
  // The first `[Skill file: …]` read that places each unit, by its key: it shows Oh My Pi loaded
  // that unit's skills, which a pool's version rule takes over the pinned build (POOL_VERSION_RULE).
  const shownBy = new Map<string, ShownRead>();
  // Each header places its unit in every context; a read by path only where no pod's links decide
  // the units (`placing`).
  for (const read of placing) {
    if (read.skill !== undefined && !readCopies.has(read.skill)) {
      const copy = run.placer.place(input.store, read.header, read.skill, ompProfile).copy;
      if (copy !== undefined) {
        const [repo = "", dir = ""] = copy.split("\t");
        readCopies.set(read.skill, { repo, path: repoPath(dir), index: read.index });
      }
    }
    if (shown.has(read.header)) continue;
    shown.add(read.header);
    const reading =
      read.byPath === true
        ? `message ${read.index} reads ${read.header} by its path`
        : `message ${read.index} reads [Skill file: ${read.header}]`;
    let rows = copies.prefixes.filter((row) => prefixMatches(row.prefix ?? "", read.header));
    let under = (row: Record<string, string>) =>
      `${reading}, under the prefix-table.tsv row ${row.prefix}`;
    if (rows.length === 0) {
      // No prefix row maps the path, so attribute.ts's rules place it, as they placed the read in
      // the count: its units are those whose prefix rows name the copy it is placed at.
      const placed =
        read.skill === undefined
          ? undefined
          : run.placer.place(input.store, read.header, read.skill, ompProfile);
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
        if (read.byPath !== true && !shownBy.has(viaKey(via))) shownBy.set(viaKey(via), read);
        if (units.has(viaKey(via))) continue;
        units.set(viaKey(via), { via, why: under(row) });
      }
    }
  }
  // The repositories the worker image's packages come from: a plugin list (omp-plugins) is the
  // pod's only when one of them holds it; another's (a dev box's own) installs nothing in a pod.
  // Their units are read at the image's commit (LEGION_IMAGE_RULE, below).
  const imageRepositories = new Set<string>();
  if (legion) {
    const ompUnits = new Set(
      copies.installers
        .filter((row) => row.host === "omp")
        .map((row) => `${row.unit} (${row.manifest_repository}:${row.manifest_path})`)
    );
    for (const row of copies.prefixes) {
      if (!(row.prefix ?? "").startsWith(IMAGE_ROOT)) continue;
      for (const via of vias(row.via ?? "")) {
        imageRepositories.add(via.repository);
        if (units.has(viaKey(via)) || !ompUnits.has(viaKey(via))) continue;
        units.set(viaKey(via), {
          via,
          why: `a Legion pod's worker image carries it: the prefix-table.tsv row ${row.prefix}`,
        });
      }
    }
    const otherLists = new Set<string>();
    for (const row of copies.installers) {
      if (row.kind !== "omp-plugins") continue;
      if (!imageRepositories.has(row.manifest_repository ?? "")) {
        const list = `${row.manifest_repository}:${row.manifest_path}`;
        if (!otherLists.has(list)) {
          otherLists.add(list);
          evidence.push({
            item: `left out: the plugin list ${list}`,
            source: `a Legion pod installs the plugins of its worker image, whose packages come from ${[...imageRepositories].sort().join(", ") || "no repository"} (the prefix-table.tsv rows under ${IMAGE_ROOT}); ${row.manifest_repository} is none of them`,
          });
        }
        continue;
      }
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

  // A dev box installs every unit its own repository's installers install into Oh My Pi at the
  // session's start, whether or not the session read from it (DEVBOX_INSTALLERS_RULE).
  evidence.push(...homeEvidence);
  // A Legion pod installs what its deployment gives each role at its start (LEGION_INSTALLS_RULE):
  // the deployment repository is the one whose project file (legion-project) names the project,
  // read at the session's start with the catalog its role setup fetches.
  const projectFile = legion
    ? copies.manifests.find(
        (row) =>
          row.kind === "legion-project" && (row.says ?? "").startsWith(`project ${input.project}:`)
      )
    : undefined;
  const deployment = projectFile?.repository;
  const legionPins = (manifests: readonly Manifest[]): InstallerPin[] => {
    if (deployment === undefined) return [];
    const deployed = legionCatalog(manifests, deployment, projectFile?.path ?? "");
    return [
      ...deployed.setup,
      ...deployed.plugins.flatMap(({ manifest, fact }) =>
        fact.repo !== "" && deployed.wanted.has(fact.plugin)
          ? [{ repository: fact.repo, ref: fact.ref, manifest }]
          : []
      ),
      // Each release the worker image takes a skill from, at its tag.
      ...factsOf(
        manifests.filter((manifest) => manifest.repository === deployment),
        "image-skill"
      ).map(({ manifest, fact }) => ({ repository: fact.repo, ref: fact.ref, manifest })),
    ];
  };

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
  if (deployment !== undefined) repositories.add(deployment);
  if (pod?.directories.some((dir) => dir.startsWith(IMAGE_ROOT))) repositories.add(pod.repo);
  // A dev-box session repository whose Claude Code settings enable plugins (CLAUDE_PLUGIN_RULE)
  // is read whole with the repositories of the marketplaces that ship them.
  if (checkout.repo !== undefined && devbox) {
    const repo = checkout.repo.toLowerCase();
    const enabled = new Set(
      copies.installers
        .filter(
          (row) =>
            (row.manifest_repository ?? "").toLowerCase() === repo &&
            row.manifest_path === CLAUDE_SETTINGS &&
            row.action === "enables"
        )
        .map((row) => row.unit ?? "")
    );
    if (enabled.size > 0) repositories.add(checkout.repo);
    for (const row of copies.installers) {
      if (!enabled.has(row.unit ?? "") || !Object.hasOwn(MEMBER_ACTIONS, row.action ?? ""))
        continue;
      repositories.add(row.manifest_repository ?? "");
      if ((row.copy_repository ?? "") !== "") repositories.add(row.copy_repository ?? "");
    }
  }
  if (home !== undefined) {
    repositories.add(home);
    for (const row of copies.installers) {
      if (
        row.manifest_repository === home &&
        row.host === "omp" &&
        Object.hasOwn(MEMBER_ACTIONS, row.action ?? "") &&
        (row.copy_repository ?? "") !== ""
      ) {
        repositories.add(row.copy_repository ?? "");
      }
    }
  }
  const held = new Map<string, RepositoryAt>();
  // The session repository is read at its commit too, for its project skills and context files;
  // it joins the units' resolution only when it is a unit's repository. A unit's repository is
  // read whole, for copies.ts's rules; the session repository where Oh My Pi reads a project
  // (`project`), whether or not it is a unit's too, since a repository's whole tree can be more
  // than GitHub gives and the project read follows the links of its skills directories.
  const toRead = new Set(repositories);
  if (checkout.repo !== undefined) toRead.add(checkout.repo);
  let projectTree: RepositoryAt | undefined;
  // The repositories' reads start together; their evidence, and the first failure, follow the
  // sorted order, as reading them one after another gives them.
  const reads = await Promise.allSettled(
    [...toRead].sort().map(async (repo) => {
      const { sha, line } =
        repo === pod?.repo && repo !== checkout.repo
          ? { sha: pod.sha, line: { item: `commit ${pod.sha} of ${repo}`, source: pod.why } }
          : await readCommit(repo);
      if (sha === undefined) return { repo, none: line };
      const { excludePaths } = run.config;
      const project =
        repo === checkout.repo ? await github.project(repo, sha, levels, excludePaths) : undefined;
      return repositories.has(repo) || project === undefined
        ? { repo, commit: { sha, line }, at: await github.at(repo, sha, excludePaths), project }
        : { repo, commit: { sha, line }, at: project.at, project };
    })
  );
  // A repository with no commit at the time it is read at did not exist then: nothing of it was
  // installed then, and as the session repository its project skills and context files are not
  // read. It is left out, and no later commit is read in its place.
  const absent = new Set<string>();
  const values = reads.flatMap((settled) => {
    if (settled.status === "rejected") throw settled.reason;
    const read = settled.value;
    if (read.none === undefined) return [read];
    absent.add(read.repo);
    const missed = [
      ...(repositories.has(read.repo)
        ? ["nothing of it was installed then, and the profile installs nothing of it"]
        : []),
      ...(read.repo === checkout.repo
        ? [
            "its project skills and context files are not read, and the profile names no session repository",
          ]
        : []),
    ];
    evidence.push({
      item: read.none.item,
      source: `${read.none.source}: GitHub holds no commit of ${read.repo} then, so ${missed.join("; ")}`,
    });
    return [];
  });
  /** `repo` at the commit it is read at, or, for one left out above, that it had none. */
  const atCommit = (repo: string) =>
    absent.has(repo) ? `${repo}, which has no commit then` : `${repo} at ${commits.get(repo)}`;
  // A unit's repository that an installer of the units pins (copies.ts's `installerPins`: a
  // `github:` dependency of an Oh My Pi plugins package.json, an `ensure_vendor` clone) is held at
  // the ref that installer names at the session's start, as the session's machine held it then;
  // repositories.tsv's branch names the ref the copies output read, later.
  const pins = isolated
    ? []
    : installerPins(
        values.flatMap((value) => (repositories.has(value.repo) ? value.at.manifests : [])),
        run.config.clones
      );
  for (const [index, value] of values.entries()) {
    const pin = pins.find((each) => each.repository.toLowerCase() === value.repo.toLowerCase());
    if (pin === undefined || pin.ref === "" || !repositories.has(value.repo)) continue;
    const when = whenOf(value.repo);
    let sha: string;
    try {
      sha = await github.commitAt(value.repo, pin.ref, when.at);
    } catch (error) {
      // A pin's tag made after the root's last prompt build: the repository stays at the commit
      // read above.
      if (transientFailure(error)) throw error;
      evidence.push({
        item: `left out: the pin ${pin.ref} of ${value.repo}`,
        source: `${pin.manifest.repository}:${pin.manifest.path} pins ${pin.ref}, which has no commit at or before ${when.words} ${when.at} (${messageFor(error)}), so ${value.repo} is read at ${value.commit.sha}`,
      });
      continue;
    }
    if (sha === value.commit.sha) continue;
    values[index] = {
      ...value,
      commit: {
        sha,
        line: {
          item: `commit ${sha} of ${value.repo}`,
          source: `the last commit of ${pin.ref}, the ref ${pin.manifest.repository}:${pin.manifest.path} pins at the session's start, at or before ${when.words} ${when.at}: ${commitsRead(value.repo, pin.ref, when.at)}`,
        },
      },
      at: await github.at(value.repo, sha, run.config.excludePaths),
    };
  }
  // A repository the dev box's installers, or a Legion deployment's role setup, pin at the
  // session's start that no unit of the copies output names (a clone an installer made then and
  // removed since, a marketplace, the source of a plugin installed from it) is read at that ref
  // too, again until no new one is pinned.
  if (home !== undefined || deployment !== undefined) {
    const tried = new Set<string>();
    for (let fresh = true; fresh; ) {
      fresh = false;
      const manifests = values.flatMap((value) =>
        repositories.has(value.repo) || value.repo === home ? value.at.manifests : []
      );
      for (const pin of [
        ...installerPins(manifests, run.config.clones),
        ...legionPins(manifests),
      ]) {
        const known = (repo: string) => repo.toLowerCase() === pin.repository.toLowerCase();
        // A pin of no ref names the default branch, which a repository left out above had no
        // commit of then.
        const leftOut = pin.ref === "" && [...absent].some(known);
        if (
          leftOut ||
          values.some((value) => known(value.repo)) ||
          tried.has(pin.repository.toLowerCase())
        ) {
          continue;
        }
        tried.add(pin.repository.toLowerCase());
        const pinned = `${pin.manifest.repository}:${pin.manifest.path}`;
        const names =
          pin.ref === "" ? "names (its default branch: the installer names no ref)" : "pins";
        const when = whenOf(pin.repository);
        let ref = pin.ref;
        try {
          if (ref === "") ref = (await github.branch(pin.repository)).branch;
          const sha = await github.commitAt(pin.repository, ref, when.at);
          repositories.add(pin.repository);
          values.push({
            repo: pin.repository,
            commit: {
              sha,
              line: {
                item: `commit ${sha} of ${pin.repository}`,
                source: `the last commit of ${ref}, the ref ${pinned} ${names} at the session's start, at or before ${when.words} ${when.at}: ${commitsRead(pin.repository, ref, when.at)}`,
              },
            },
            at: await github.at(pin.repository, sha, run.config.excludePaths),
            project: undefined,
          });
          fresh = true;
        } catch (error) {
          if (transientFailure(error)) throw error;
          evidence.push(
            error instanceof NoCommitThen
              ? {
                  item: `left out: ${pin.repository}, which has no commit at or before ${when.words} ${when.at}`,
                  source: `${ref}, the ref ${pinned} ${names} at the session's start, has no commit at or before ${when.words}: ${commitsRead(pin.repository, ref, when.at)} gives none, so nothing of ${pin.repository} was installed then, and the profile installs nothing of it`,
                }
              : {
                  item: `left out: ${pin.repository}, which ${pinned} pins at the session's start`,
                  source: messageFor(error),
                }
          );
        }
      }
    }
  }
  for (const { repo, commit, at, project } of values) {
    commits.set(repo, commit.sha);
    evidence.push(commit.line);
    const { byParent, rel } = checkout;
    if (
      repo === checkout.repo &&
      byParent !== undefined &&
      rel !== "" &&
      (project?.missing ?? []).includes(rel)
    ) {
      const line = evidence.indexOf(checkout.line);
      const named = `${DEVBOX_CHECKOUT_RULE}. The working directory ${input.cwd} is under ${byParent}, named as ${repo}, whose tree at ${commit.sha} holds no ${rel}`;
      // Directly inside that parent, it is a workspace of the repository at its root where a file
      // the session read by an absolute path under it is a file of the tree at its path there.
      const direct = !rel.includes("/");
      const { cwd } = input;
      const underCwd =
        !direct || cwd === undefined
          ? []
          : input.absoluteReads.flatMap((read) => {
              const file = path.posix.relative(cwd, read.path);
              return file === "" || file === ".." || file.startsWith("../")
                ? []
                : [{ ...read, file }];
            });
      const files = [...new Set(underCwd.map((read) => read.file))];
      const first =
        files.length === 0 ? undefined : await github.firstFile(repo, commit.sha, files);
      const shown = underCwd.find((read) => read.file === first);
      if (shown !== undefined) {
        checkout = {
          repo,
          rel: "",
          line: {
            item: `${SESSION_REPOSITORY_ITEM}${repo}`,
            source: `${named}; it is a workspace of ${repo} named ${rel}, a checkout of it at its root: message ${shown.index} reads ${shown.path}, and the tree holds ${shown.file}`,
          },
        };
        evidence[line] = checkout.line;
        levels = [""];
        projectDirs = projectDirsAt(levels);
      } else {
        notCheckout = true;
        const readCount =
          files.length === 0 ? "none" : `${files.length} such file${files.length === 1 ? "" : "s"}`;
        evidence[line] = {
          item: NO_SESSION_REPOSITORY_ITEM,
          source: `${named}, so that name alone does not make the directory a checkout of it; and ${
            direct
              ? `no file the session read by an absolute path under it is a file of that tree at its path there (it read ${readCount}), so nothing shows it is a workspace of ${repo}`
              : `it is not directly inside ${byParent}, so it is not taken as a workspace of ${repo}`
          }`,
        };
      }
    }
    if (repo === checkout.repo && !notCheckout) projectTree = project?.at ?? at;
    const unitRepo = repositories.has(repo);
    if (unitRepo) held.set(repo, at);
    const projectRead = `gh api repos/${repo}/git/trees/<tree id>, one directory a read (${project?.reads ?? 0} ${(project?.reads ?? 0) === 1 ? "read" : "reads"}), of the directories Oh My Pi reads a project's skills and context files from: each level from the working directory up to the checkout's root, its ${PROJECT_LEVEL_DIRS.map((dir) => `${dir}/`).join(", ")}, its ${PROJECT_SKILLS.flatMap((kind) => kind.dirs.map((dir) => `${dir}/`)).join(", ")} and each skill directory in them, a skill directory held as a link read at the directory it names. Their ${(project?.at ?? at).copies.length} SKILL.md files, read at that commit, give the session repository's project skills`;
    evidence.push(
      unitRepo
        ? {
            item: `the skills and manifests of ${repo} at ${commit.sha}`,
            source: `gh api repos/${repo}/git/trees/${commit.sha}?recursive=1: its ${at.copies.length} SKILL.md files and ${at.manifests.length} manifest files, read at that commit and resolved by copies.ts's rules (resolveInstallers), give what each unit ships then${project === undefined ? "" : `; and ${projectRead}`}`,
          }
        : { item: `the skills of ${repo} at ${commit.sha}`, source: projectRead }
    );
    // Each link of a project skills directory: followed to the directory it names in the
    // repository, as Oh My Pi's scan follows it, or left out where it names none there.
    for (const { link, target, outcome } of project?.links ?? []) {
      evidence.push(
        outcome === "followed"
          ? {
              item: `linked: ${repo} ${link} -> ${target}`,
              source: `${repo} at ${commit.sha} holds ${link} as a symbolic link to ${target}, which Oh My Pi follows (discovery/helpers.ts, lines 579-580 and 594-596): the skill directory it names is read under the link's path`,
            }
          : {
              item: `left out: ${repo} ${link} -> ${target}`,
              source: `${repo} at ${commit.sha} holds ${link} as a symbolic link to ${target}, ${outcome === "outside" ? "outside the repository" : "which names no directory of the repository"}, so no repository read gives what it names`,
            }
      );
    }
    for (const fault of at.faults) {
      const dir = path.posix.dirname(path.posix.dirname(fault.file));
      if (!unitRepo && !projectDirs.some((each) => each.dir === dir)) continue;
      evidence.push({
        item: `left out: ${repo} ${fault.file} at ${commit.sha}`,
        source: fault.why,
      });
    }
  }
  // Each tree read at another commit than its repository's (a pinned package's release tag, a
  // pool host's at a subagent's start), by `<repo>@<commit>`.
  const trees = new Map<string, RepositoryAt>();
  // A Legion pod's worker image carries the packages of its repository as that repository was at
  // the commit the image was built from (LEGION_IMAGE_RULE), not at its default branch at the
  // session's start: the units of that repository are resolved from its tree at that commit
  // (`unitTree`), and their copies name it. The repository stays read at the session's start for
  // anything else of it (a session repository's project skills and context files, a copy a read
  // in its checkout is placed at). Its daemon image, which composes the role prompt, may be
  // another build: its commit is the evidence item DAEMON_IMAGE_ITEM, which context.ts reads
  // (LEGION_DAEMON_RULE).
  const [imageRepo, ...moreImageRepos] = [...imageRepositories].sort();
  let image: { readonly repo: string; readonly sha: string; readonly at: RepositoryAt } | undefined;
  if (pod !== undefined && repositories.has(pod.repo)) {
    const at = await github.at(pod.repo, pod.sha, run.config.excludePaths);
    image = { repo: pod.repo, sha: pod.sha, at };
    trees.set(`${pod.repo}@${pod.sha}`, at);
  }
  if (deployment !== undefined && imageRepo !== undefined) {
    const depSha = commits.get(deployment);
    if (moreImageRepos.length > 0 || depSha === undefined) {
      throw new PodNotMeasurable(
        `${
          moreImageRepos.length > 0
            ? `the worker image's packages come from ${imageRepositories.size} repositories (the prefix-table.tsv rows under ${IMAGE_ROOT}), so which one the image's commit is of is not known`
            : `${deployment} has no commit at the session's start, so it names no image`
        }, and a repository's default branch is a guess, so the profile is not built: ${LEGION_IMAGE_RULE}`
      );
    } else {
      const read = {
        deployment,
        sha: depSha,
        projectPath: projectFile?.path ?? "",
        project: input.project ?? "",
        imageRepo,
        when: whenOf(deployment),
        deploymentImages: run.config.deploymentImages ?? [],
      };
      const worker = await imageCommit(github, { ...read, key: "image" });
      const daemon = await imageCommit(github, { ...read, key: "daemonImage" });
      if (worker.sha === undefined) {
        throw new PodNotMeasurable(
          `the worker image's commit of ${imageRepo} is not known, and the repository's default branch is a guess, so the profile is not built: ${worker.how}: ${LEGION_IMAGE_RULE}`
        );
      }
      evidence.push(
        {
          item: `the worker image's commit of ${imageRepo}`,
          source: `${worker.how}, so the image's units of ${imageRepo} are read at ${worker.sha}: ${LEGION_IMAGE_RULE}`,
        },
        {
          item: `${DAEMON_IMAGE_ITEM}${imageRepo}`,
          source:
            daemon.sha === undefined
              ? `${daemon.how}, so the role prompt is composed at the commit the profile reads the plugins of ${imageRepo} at: ${LEGION_DAEMON_RULE}`
              : `${daemon.how}, ${composedAt(imageRepo, daemon.sha)}: ${LEGION_DAEMON_RULE}`,
        }
      );
      if (
        worker.sha !== undefined &&
        worker.sha !== commits.get(imageRepo) &&
        held.has(imageRepo)
      ) {
        const at = await github.at(imageRepo, worker.sha, run.config.excludePaths);
        image = { repo: imageRepo, sha: worker.sha, at };
        trees.set(`${imageRepo}@${worker.sha}`, at);
        evidence.push({
          item: `the skills and manifests of ${imageRepo} at ${worker.sha}`,
          source: `gh api repos/${imageRepo}/git/trees/${worker.sha}?recursive=1: its ${at.copies.length} SKILL.md files and ${at.manifests.length} manifest files, read at the worker image's commit and resolved by copies.ts's rules (resolveInstallers), give what each unit of ${imageRepo} ships in the image`,
        });
      }
    }
  }
  /** The tree a unit of `repo` ships from: the worker image's at its commit for the image's
   *  repository, else `at`, the tree held at the session's start. */
  const unitTree = (repo: string, at: RepositoryAt) => (repo === image?.repo ? image.at : at);
  // A subagent's pool hosts at its start: Oh My Pi rescans a pool's directories then, while the
  // host's plugin lists, installers and settings are what its root loaded (PARENT_LOAD_RULE).
  const poolAt = new Map<string, string>();
  if (readTime?.at !== undefined && readTime.poolsAtStart) {
    for (const repo of [...held.keys()].sort()) {
      if (!poolHosts.has(repo.toLowerCase())) continue;
      const { branch } = await github.branch(repo);
      const sha = await github.commitAt(repo, branch, input.sessionStart);
      evidence.push({
        item: `${READ_TIMES_ITEM}${repo}`,
        source: `its plugin lists, installers and settings at ${commits.get(repo)} (the root's last prompt build ${readTime.at}), its pool directories at ${sha}, the last commit of ${branch} at or before the subagent's start ${input.sessionStart}: ${PARENT_LOAD_RULE}`,
      });
      if (sha === commits.get(repo)) continue;
      poolAt.set(repo, sha);
      trees.set(`${repo}@${sha}`, await github.at(repo, sha, run.config.excludePaths));
    }
  }
  // A package a plugin list pins by an exact npm version ships what that version ships
  // (NPM_PIN_RULE); the dev box's own plugin lists come first.
  const versions = new Map<string, PinnedVersion>();
  const heldManifests = [...held].flatMap(([repo, at]) => unitTree(repo, at).manifests);
  const lists = heldManifests.toSorted(
    (a, b) => Number(b.repository === home) - Number(a.repository === home)
  );
  for (const pin of isolated ? [] : npmPins(lists)) {
    const id = `package:${pin.package}`;
    if (versions.has(id)) continue;
    const pkg = heldManifests.find(
      (manifest) =>
        manifest.kind === "omp-package" &&
        manifest.facts.some((fact) => fact.type === "package" && fact.name === pin.package)
    );
    if (pkg === undefined) continue;
    const repo = pkg.repository;
    const tag = releaseTag(pkg.path, pin.version);
    const pinned = `${pin.manifest.repository}:${pin.manifest.path} at ${commits.get(pin.manifest.repository)} pins ${pin.package} ${pin.version}`;
    const listing = `gh api repos/${repo}/git/matching-refs/tags/${tag}`;
    let tagWhy = `GitHub lists no tag ${tag} of ${repo} (${listing})`;
    try {
      if (await github.hasTag(repo, tag)) {
        const sha = await github.commitAt(repo, tag, whenOf(repo).at);
        const at = await github.at(repo, sha, run.config.excludePaths);
        if (at.manifests.some((each) => each.path === pkg.path && each.kind === "omp-package")) {
          trees.set(`${repo}@${sha}`, at);
          const rows = resolveInstallers(
            at.manifests,
            new CopyIndex(at.copies),
            run.config.clones
          ).rows.filter(
            (row) =>
              row.unit === id &&
              row.manifestRepository === repo &&
              row.manifestPath === pkg.path &&
              shipsMember(row)
          );
          versions.set(id, {
            sha,
            rows,
            why: `${pinned}; its release tag ${tag} (${listing}) is ${repo} at ${sha}, whose tree holds ${pkg.path}: ${NPM_PIN_RULE}`,
          });
          continue;
        }
        tagWhy = `the tree of ${repo}'s tag ${tag} (${sha}) holds no ${pkg.path}`;
      }
    } catch (error) {
      if (transientFailure(error)) throw error;
      tagWhy = `${repo}'s tag ${tag} cannot be read (${messageFor(error)})`;
    }
    const url = tarballUrl(pin.package, pin.version);
    const names = await github.shippedSkills(pin.package, pin.version);
    if (names === undefined) {
      versions.set(id, {
        why: `${pinned}; ${tagWhy}, and the registry has no tarball of it (${url}), so it ships nothing to the profile: ${NPM_PIN_RULE}`,
      });
      continue;
    }
    versions.set(id, {
      names,
      why: `${pinned}; ${tagWhy}, so the registry's tarball (${url}) names the skills it ships (${[...names].join(", ") || "none"}), each installed from ${repo} at ${commits.get(repo)}: ${NPM_PIN_RULE}`,
    });
  }
  const resolved = resolveInstallers(
    heldManifests,
    new CopyIndex([...held].flatMap(([repo, at]) => unitTree(repo, at).copies)),
    run.config.clones
  );
  /** The tree of `repo` a pool's directories are read at: its own at a subagent's start, else
   *  the one held. */
  const poolTree = (repo: string, at: RepositoryAt) =>
    trees.get(`${repo}@${poolAt.get(repo) ?? ""}`) ?? at;
  const poolResolved =
    poolAt.size === 0
      ? resolved
      : resolveInstallers(
          [...held].flatMap(([repo, at]) => poolTree(repo, unitTree(repo, at)).manifests),
          new CopyIndex(
            [...held].flatMap(([repo, at]) => poolTree(repo, unitTree(repo, at)).copies)
          ),
          run.config.clones
        );
  if (pod !== undefined) {
    for (const row of resolved.rows) {
      if (row.manifestRepository !== pod.repo || row.kind !== "omp-package" || !shipsMember(row))
        continue;
      const directory = `${IMAGE_ROOT}${path.posix.basename(path.posix.dirname(row.manifestPath))}`;
      if (!pod.directories.includes(directory)) continue;
      const via = {
        unit: row.unit,
        repository: row.manifestRepository,
        manifest: row.manifestPath,
      };
      units.set(viaKey(via), { via, why: `${directory} is linked into ${ompProfile}: ${pod.why}` });
    }
  }
  /** The rows by which `via`'s unit ships a skill at the session's start (copies.ts's rules): a
   *  pool's from its host's tree at the subagent's start. */
  const shipsAtStart = (via: Via) =>
    (via.unit.startsWith("pool:") ? poolResolved : resolved).rows.filter(
      (row) =>
        row.unit === via.unit &&
        row.manifestRepository === via.repository &&
        row.manifestPath === via.manifest &&
        shipsMember(row)
    );
  /** What the profile installs of a unit that ships at the session's start: `shipsAtStart`'s rows,
   *  narrowed to those its pinned npm version ships (`version`, NPM_PIN_RULE) where it has one. */
  const installedFrom = (via: Via, version: PinnedVersion | undefined) => {
    const rows = shipsAtStart(via);
    if (version === undefined) return rows;
    return (
      version.rows ??
      (version.names === undefined
        ? []
        : rows.filter((row) => version.names?.has(row.skill) === true))
    );
  };
  // A read whose skill no unit ships at the session's start, though the repository its unit comes
  // from was read at that commit: the copies output names the unit as the manifests name it at
  // the later commit it read, and a marketplace or a plugin renamed since leaves that unit empty
  // then (the read's path names the plugin cache directory `<marketplace>___<plugin>___<version>`
  // of the name it had).
  // The prefix rows the same manifests give at the session's start (copies.ts's rules,
  // `resolved.prefixes`) place the read at the unit that installed it then, and at its copy.
  const placedAtStart = new Set<string>();
  for (const read of placing) {
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
        if (read.byPath !== true && !shownBy.has(viaKey(via))) shownBy.set(viaKey(via), read);
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

  // The dev box's own installers at the session's start: each unit they install into Oh My Pi,
  // after the units the reads come from, so a read's unit keeps its place among those of a name.
  // A pool's rows are its host's at the subagent's start (`poolResolved`), a pool that host names
  // then and not at the root's build among them; each pool is decided below.
  if (home !== undefined) {
    const atBuild = new Set(resolved.rows.map((row) => `${row.unit}\0${row.manifestPath}`));
    const rows = [
      ...resolved.rows.filter((row) => !row.unit.startsWith("pool:")),
      ...poolResolved.rows.filter((row) => row.unit.startsWith("pool:")),
    ];
    for (const row of rows) {
      if (row.manifestRepository !== home || row.host !== "omp" || !shipsMember(row)) {
        continue;
      }
      const via = { unit: row.unit, repository: home, manifest: row.manifestPath };
      if (units.has(viaKey(via))) continue;
      units.set(viaKey(via), {
        via,
        why: atBuild.has(`${row.unit}\0${row.manifestPath}`)
          ? `${DEVBOX_INSTALLERS_RULE}: ${home}:${row.manifestPath} at ${commits.get(home)} ${row.action} it into Oh My Pi`
          : `pool at the subagent's start: ${home}:${row.manifestPath} names ${row.unit.slice("pool:".length)} at ${poolAt.get(home)} (the subagent's start ${input.sessionStart}) and not at ${commits.get(home)} (the root's last prompt build)`,
      });
    }
  }
  // The Claude Code plugins the session repository's own Claude Code settings enable, which
  // Claude Code installs for that project and Oh My Pi lists for a session in it
  // (CLAUDE_PLUGIN_RULE): each from the marketplace rows that ship its skills.
  if (checkout.repo !== undefined && devbox && !notCheckout) {
    const repo = checkout.repo.toLowerCase();
    const enabled = new Set(
      resolved.rows
        .filter(
          (row) =>
            row.manifestRepository.toLowerCase() === repo &&
            row.manifestPath === CLAUDE_SETTINGS &&
            row.action === "enables"
        )
        .map((row) => row.unit)
    );
    for (const row of resolved.rows) {
      if (!enabled.has(row.unit) || !shipsMember(row)) {
        continue;
      }
      const via = {
        unit: row.unit,
        repository: row.manifestRepository,
        manifest: row.manifestPath,
      };
      if (units.has(viaKey(via))) continue;
      units.set(viaKey(via), {
        via,
        why: `${CLAUDE_PLUGIN_RULE}: ${checkout.repo}:${CLAUDE_SETTINGS} at ${commits.get(checkout.repo)} enables ${row.unit.slice("plugin:".length)} in enabledPlugins, and ${row.manifestRepository}:${row.manifestPath} ${row.action} its skills`,
      });
    }
  }
  // A Legion pod's role start installs the catalog plugins its deployment gives the project, and
  // its worker image carries the skills its Dockerfile installs under /opt.
  if (deployment !== undefined) {
    const deployed = legionCatalog(
      [...held.values()].flatMap((at) => at.manifests),
      deployment,
      projectFile?.path ?? ""
    );
    if (deployed.setup.length === 0) {
      evidence.push({
        item: "left out: the catalog of the Legion role setup",
        source: `${LEGION_INSTALLS_RULE}; ${atCommit(deployment)} holds no role setup script that names a catalog`,
      });
    }
    for (const row of resolved.rows) {
      const plugin = row.unit.startsWith("plugin:")
        ? (row.unit.slice("plugin:".length).split("@")[0] ?? "")
        : "";
      const catalog = deployed.setup.some(
        (pin) => pin.repository.toLowerCase() === row.manifestRepository.toLowerCase()
      );
      const fromCatalog =
        catalog && row.kind === "claude-marketplace" && deployed.wanted.has(plugin);
      const fromImage = row.manifestRepository === deployment && row.unit.startsWith("image:");
      if (!(fromCatalog || fromImage) || !shipsMember(row)) {
        continue;
      }
      const via = {
        unit: row.unit,
        repository: row.manifestRepository,
        manifest: row.manifestPath,
      };
      if (units.has(viaKey(via))) continue;
      units.set(viaKey(via), {
        via,
        why: fromCatalog
          ? `${LEGION_INSTALLS_RULE}: ${deployment}'s role setup installs ${plugin} of the catalog ${row.manifestRepository} (${deployed.why})`
          : `${LEGION_INSTALLS_RULE}: ${deployment}:${row.manifestPath} at ${commits.get(deployment)} installs it in the worker image`,
      });
    }
  }

  // Each pool an extension's resources_discover gives: decided by the recorded listing where the
  // session has one (POOL_LISTING_RULE), else by the Oh My Pi build its dev box pinned at its start
  // (POOL_VERSION_RULE), and kept as it came where neither decides or the caller asks for every
  // pool (`poolsUndecided`). The names of a pool the listing decides are marked as taken from it.
  // A name an installed unit outside the pools also ships is no evidence for a pool (the session
  // lists it with or without the pool): a pool needs a named skill that no such unit ships, or a
  // `[Skill file: …]` read of the session that shows Oh My Pi loaded it.
  const fromListing = new Set<string>();
  const pools =
    input.poolsUndecided === true
      ? []
      : [...units].filter(([, { via }]) => via.unit.startsWith("pool:"));
  const listing = input.recordedListing;
  const { ompPin } = run.config;
  if (listing !== undefined) {
    const inference =
      "this is an inference from the listing, since no file records why some sessions of one Oh My Pi build list such a pool and others do not";
    const nonPoolNames = new Set<string>();
    for (const { via } of units.values()) {
      if (via.unit.startsWith("pool:")) continue;
      for (const row of installedFrom(via, versions.get(via.unit))) nonPoolNames.add(row.skill);
    }
    for (const copy of alone.values()) {
      const found = held.get(copy.repo)?.copies.find((each) => each.path === copy.path);
      if (found !== undefined) nonPoolNames.add(found.name);
    }
    for (const [key, { via, why }] of pools) {
      const skills = [...new Set(shipsAtStart(via).map((row) => row.skill))];
      const named = skills.filter((name) => listing.names.includes(name));
      const unnamed = skills.filter((name) => !named.includes(name));
      const unique = named.filter((name) => !nonPoolNames.has(name));
      const shared = named.filter((name) => nonPoolNames.has(name));
      const counted = `${listing.from} names ${named.length === 0 ? "none" : named.length} of the ${skills.length} skills it holds at the session's start`;
      const split = `named: ${named.join(", ") || "none"}; unnamed: ${unnamed.join(", ") || "none"}`;
      const sharedNote =
        shared.length === 0
          ? ""
          : `; named skills that an installed unit outside the pools also ships give no evidence for this pool (${shared.join(", ")})`;
      if (unique.length > 0) {
        fromListing.add(key);
        units.set(key, {
          via,
          why: `${why}; ${counted} (${split})${sharedNote}, so it is installed${shared.length === 0 ? "" : ` on ${unique.join(", ")}`}; ${inference}${unnamed.length === 0 ? "" : ", and the listing measure counts its unnamed skills as added names"}: ${POOL_LISTING_RULE}`,
        });
        continue;
      }
      const read = shownBy.get(key);
      if (read === undefined) {
        units.delete(key);
        evidence.push({
          item: `left out: the pool ${key}`,
          source: `${why}; ${counted} (${skills.join(", ") || "none"})${sharedNote}, so it is not installed; ${inference}: ${POOL_LISTING_RULE}`,
        });
        continue;
      }
      units.set(key, {
        via,
        why: `${why}; ${counted} (${split})${sharedNote}; message ${read.index} reads [Skill file: ${read.header}] of it, so Oh My Pi loaded it, which is direct proof and no inference from the listing, so it is installed and none of its names is marked fromRecorded: ${POOL_LISTING_RULE}`,
      });
    }
  } else if (home !== undefined && ompPin !== undefined && pools.length > 0) {
    const pinned = await pinnedOmp(github, home, ompPin, input.sessionStart);
    const first = ompPin.firstPoolVersion;
    if (pinned.version === undefined) {
      evidence.push({
        item: `${OMP_PIN_ITEM}${home}`,
        source: `${pinned.read}, so no pinned build decides the pools, and each is installed as the dev box's installers install it: ${POOL_VERSION_RULE}`,
      });
    } else {
      const loads = Bun.semver.order(pinned.version, first) >= 0;
      const order = `${loads ? "at or after" : "before"} ${first}`;
      const release = releaseOf(pinned.version);
      const { runtime } = input;
      const cross =
        runtime === undefined
          ? "the session's file records no Oh My Pi version, so nothing checks the pin"
          : `the session's file records Oh My Pi ${runtime} (the runtime.version of its first model_change line), which ${releaseOf(runtime) === release ? "agrees with" : "differs from"} the pin's release ${release}`;
      evidence.push({
        item: `${OMP_PIN_ITEM}${home}`,
        source: `${pinned.read}, which is ${order}, the first build the configuration's ompPin names as loading resources_discover pools on a fresh start; ${cross}: ${POOL_VERSION_RULE}`,
      });
      const pinWords = `the pinned Oh My Pi ${pinned.version} is ${order}`;
      for (const [key, { via, why }] of pools) {
        const read = shownBy.get(key);
        if (loads || read !== undefined) {
          units.set(key, {
            via,
            why: `${why}; ${pinWords}, ${
              loads
                ? "which loads it on a fresh start"
                : `but message ${read?.index} reads [Skill file: ${read?.header}] of it, so Oh My Pi loaded it`
            }: ${POOL_VERSION_RULE}`,
          });
          continue;
        }
        units.delete(key);
        evidence.push({
          item: `left out: the pool ${key}`,
          source: `${why}; ${pinWords}, so the session listed none of its skills on a fresh start; an inference from the pinned build: ${POOL_VERSION_RULE}`,
        });
      }
    }
  }

  const plugins = new Map<string, { repo: string; path: string; name: string; why: string }>();
  /** Each copy a unit ships at the session's start, in the order of the units, with how it ships,
   *  the evidence of it and its rank among the copies of its name (`unitRank`); then each copy
   *  installed alone, and then the session repository's project skills. */
  const offers: {
    copy: Omit<ProfileSkill, "commit"> & { readonly commit?: string };
    how: string;
    source: string;
    rank: number | undefined;
    /** The namespace Oh My Pi lists a second copy of the name under (`unitNamespace`). */
    namespace?: string | undefined;
    /** Whether its unit is a pool the recorded listing decided (POOL_LISTING_RULE). */
    fromRecorded?: boolean;
  }[] = [];
  for (const [key, { via, why }] of units) {
    const id = via.unit;
    const start =
      via.repository === image?.repo
        ? `${image.repo} at ${image.sha} (the worker image's commit)`
        : atCommit(via.repository);
    const version = id.startsWith("package:") ? versions.get(id) : undefined;
    const members = installedFrom(via, version);
    if (version !== undefined) evidence.push({ item: `version of ${key}`, source: version.why });
    const later = listed(via);
    evidence.push({ item: `unit ${key}`, source: why });
    if (members.length === 0) {
      evidence.push({
        item: `left out: the skills of ${key}`,
        source: `${start}: by copies.ts's rules ${via.manifest} ships no skill there`,
      });
    }
    for (const row of members) {
      const commit =
        version?.sha ??
        (id.startsWith("pool:") ? poolAt.get(row.copyRepository) : undefined) ??
        (row.copyRepository === image?.repo ? image.sha : undefined);
      const copy = {
        repo: row.copyRepository,
        path: row.copyPath ?? "",
        name: row.skill,
        ...(commit === undefined ? {} : { commit }),
      };
      const how = `${id} ${row.action} it (${row.manifestRepository}:${row.manifestPath})`;
      const inR4 = later.some(
        (each) => each.copy_repository === copy.repo && repoPath(each.copy_path ?? "") === copy.path
      );
      offers.push({
        copy,
        how,
        source: `${copy.repo} at ${commit ?? commits.get(copy.repo)}: ${how}${inR4 ? "" : "; installers.tsv, which the copies output read later, does not list it"}`,
        rank: unitRank(id),
        namespace: unitNamespace(id),
        fromRecorded: fromListing.has(key),
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
        // A package at its repository's root (a `github:` dependency's) is named as Oh My Pi
        // installs it, `node_modules/<name>`: its unit's name, the last part of a scoped one.
        const name =
          dir === "."
            ? (id.slice("package:".length).split("/").pop() ?? "")
            : path.posix.basename(dir);
        return { repo: manifest.manifestRepository, path: dir, name };
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
  const same = (a: { repo: string; path: string }, b: { repo: string; path: string }) =>
    a.repo === b.repo && a.path === b.path;
  // The session repository's project skills, in the order Oh My Pi admits them (projectDirs).
  const projectOffers: (typeof offers)[number][] = [];
  if (checkout.repo !== undefined && projectTree !== undefined) {
    const how = `project skill of ${checkout.repo}`;
    const start = `${how} at ${commits.get(checkout.repo)}`;
    for (const { dir, rank, how: listing, namespace, describedOnly } of projectDirs) {
      for (const copy of projectTree.copies) {
        if (path.posix.dirname(copy.path) !== dir) continue;
        // A SKILL.md that could not be read is named in the faults' evidence, and is offered.
        if (
          describedOnly &&
          copy.description === "" &&
          !copy.fault.startsWith("SKILL.md read failed")
        ) {
          evidence.push({
            item: `left out: skill ${copy.name} from ${checkout.repo} ${copy.path}`,
            source: `${start}: its SKILL.md has no description, and Oh My Pi lists no skill of ${path.posix.basename(path.posix.dirname(dir))}/skills/ without one (requireDescription: discovery/builtin.ts, line 324; discovery/github.ts, line 294; discovery/helpers.ts, line 543)`,
          });
          continue;
        }
        projectOffers.push({
          copy: { repo: checkout.repo, path: copy.path, name: copy.name },
          how,
          source: `${start}: ${listing}`,
          rank,
          namespace,
        });
      }
    }
  }
  for (const copy of alone.values()) {
    // A read of a project skill is placed at its copy, which the project skills hold already.
    if (projectOffers.some((each) => same(each.copy, copy))) continue;
    const start = atCommit(copy.repo);
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
      rank: undefined,
    });
  }
  offers.push(...projectOffers);
  // The dev box's Oh My Pi config, read at the session's start: Oh My Pi loads no skill whose name
  // a pattern of its skills.ignoredSkills matches (IGNORED_RULE, copies.ts's `ignoredSkillMatch`).
  const ignores =
    home === undefined
      ? []
      : factsOf(held.get(home)?.manifests ?? [], "ignores").flatMap(({ manifest, fact }) =>
          fact.patterns.map((pattern) => ({
            pattern,
            file: manifest.path,
            matches: ignoredSkillMatch(pattern),
          }))
        );
  for (let at = offers.length - 1; at >= 0 && ignores.length > 0; at--) {
    const offer = offers[at];
    const by = ignores.find(({ matches }) => matches(offer?.copy.name ?? ""));
    if (offer === undefined || by === undefined) continue;
    offers.splice(at, 1);
    const inProject = projectOffers.indexOf(offer);
    if (inProject >= 0) projectOffers.splice(inProject, 1);
    evidence.push({
      item: `left out: skill ${offer.copy.name} (${offer.copy.repo} ${offer.copy.path})`,
      source: `${offer.how}; ${home}:${by.file} at ${commits.get(home ?? "")} names ${by.pattern} in skills.ignoredSkills: ${IGNORED_RULE}`,
    });
  }

  // Each name's copy: the one the session read, when a unit ships it at the session's start (the
  // read shows which of the copies of one name the host gave the session); else a project skill
  // that Oh My Pi admits before every other copy of its name (PROJECT_PRECEDENCE); else the copy
  // of the unit Oh My Pi admits first (unitRank), of units of one rank the first unit's. The
  // others of the name are left out, or listed under a namespace (NAMESPACE_RULE).
  const chosen = new Map<string, (typeof offers)[number]>();
  for (const [name, read] of readCopies) {
    const offer = offers.find((each) => each.copy.name === name && same(each.copy, read));
    if (offer !== undefined) chosen.set(name, offer);
  }
  for (const [at, offer] of projectOffers.entries()) {
    const { name } = offer.copy;
    // Of two project skills, the one Oh My Pi reads first (projectDirs); of a project skill and a
    // unit's, the one of the lower rank.
    const first = offers.every((each) => {
      if (each.copy.name !== name || same(each.copy, offer.copy)) return true;
      const other = projectOffers.indexOf(each);
      if (other >= 0) return other > at;
      return each.rank !== undefined && offer.rank !== undefined && offer.rank < each.rank;
    });
    if (first && !chosen.has(name)) chosen.set(name, offer);
  }
  const rankOf = (offer: (typeof offers)[number]) => offer.rank ?? Number.POSITIVE_INFINITY;
  for (const offer of offers) {
    const { name } = offer.copy;
    if (chosen.has(name)) continue;
    const first = offers
      .filter((each) => each.copy.name === name)
      .reduce((a, b) => (rankOf(b) < rankOf(a) ? b : a));
    chosen.set(name, first);
  }
  const skills = new Map<string, Omit<ProfileSkill, "commit"> & { readonly commit?: string }>();
  const again: InstallPath[] = [];
  const fromRecorded = new Set<string>();
  /** The tree a copy is read from: its own commit's when it names one, else the session
   *  repository's project read when it holds the copy (a linked project skill is held there
   *  alone), else its repository's. */
  const treeOf = (copy: { repo: string; path?: string; commit?: string | undefined }) =>
    (copy.commit === undefined ? undefined : trees.get(`${copy.repo}@${copy.commit}`)) ??
    (copy.repo === checkout.repo &&
    projectTree?.copies.some((each) => each.path === (copy.path === "." ? "" : copy.path))
      ? projectTree
      : undefined) ??
    held.get(copy.repo) ??
    (copy.repo === checkout.repo ? projectTree : undefined);
  /** The blob of the SKILL.md of `copy` at the session's start, as read. */
  const blobOf = (copy: { repo: string; path: string; commit?: string | undefined }) =>
    treeOf(copy)?.copies.find((each) => each.path === copy.path)?.blob;
  for (const offer of offers) {
    const pick = chosen.get(offer.copy.name);
    if (pick === undefined) continue;
    const read = readCopies.get(offer.copy.name);
    const readAt = read !== undefined && same(pick.copy, read) ? read.index : undefined;
    if (pick === offer) {
      skills.set(offer.copy.name, offer.copy);
      if (offer.fromRecorded === true) fromRecorded.add(offer.copy.name);
      evidence.push({
        item: `skill ${offer.copy.name} (${offer.copy.repo} ${offer.copy.path})`,
        source: `${offer.source}${readAt === undefined ? "" : `; message ${readAt} reads this copy`}`,
      });
    } else if (!same(pick.copy, offer.copy)) {
      const why =
        readAt !== undefined
          ? `the session read the copy of ${pick.copy.repo} ${pick.copy.path} (message ${readAt})`
          : projectOffers.includes(pick) ||
              projectOffers.includes(offer) ||
              rankOf(pick) < rankOf(offer)
            ? `Oh My Pi gives the session the copy of ${pick.copy.repo} ${pick.copy.path} under that name (${pick.how}): ${PROJECT_PRECEDENCE}`
            : `the copy of ${pick.copy.repo} ${pick.copy.path} came first`;
      // A second copy of the name whose text differs Oh My Pi lists under its namespace, and one of
      // the same text it drops (NAMESPACE_RULE).
      const blob = blobOf(offer.copy);
      const differs = blob !== undefined && blob !== blobOf(pick.copy);
      const namespaced =
        offer.namespace === undefined
          ? undefined
          : listedName({ name: offer.copy.name, namespace: offer.namespace });
      // Oh My Pi matches skills.ignoredSkills against the name it would list the copy under too,
      // and loads none it matches (IGNORED_RULE).
      const ignoredAs =
        namespaced === undefined ? undefined : ignores.find(({ matches }) => matches(namespaced));
      if (differs && namespaced !== undefined && ignoredAs !== undefined) {
        evidence.push({
          item: `left out: skill ${namespaced} (${offer.copy.repo} ${offer.copy.path})`,
          source: `${offer.source}; under the name ${offer.copy.name} ${why}, and this copy's text differs, so Oh My Pi would list it as ${namespaced} (${NAMESPACE_RULE}); ${home}:${ignoredAs.file} at ${commits.get(home ?? "")} names ${ignoredAs.pattern} in skills.ignoredSkills: ${IGNORED_RULE}`,
        });
        continue;
      }
      if (differs && namespaced !== undefined && !skills.has(namespaced)) {
        skills.set(namespaced, { ...offer.copy, namespace: offer.namespace });
        if (offer.fromRecorded === true) fromRecorded.add(namespaced);
        evidence.push({
          item: `skill ${namespaced} (${offer.copy.repo} ${offer.copy.path})`,
          source: `${offer.source}; under the name ${offer.copy.name} ${why}, and this copy's text differs: ${NAMESPACE_RULE}`,
        });
        continue;
      }
      let text = "";
      if (differs) {
        text =
          namespaced === undefined
            ? "; its SKILL.md differs, and the namespace Oh My Pi would list it under is not known"
            : `; its SKILL.md differs, and ${namespaced} is taken`;
      } else if (blob !== undefined) {
        text = "; its SKILL.md is the same, and Oh My Pi drops such a copy (NAMESPACE_RULE)";
      }
      evidence.push({
        item: `left out: skill ${offer.copy.name} from ${offer.copy.repo} ${offer.copy.path}`,
        source: `${offer.how} too; ${why}${text}`,
      });
    } else if (offer.namespace !== undefined) {
      again.push({ copy: offer.copy, how: offer.how, namespace: offer.namespace });
    }
  }
  const shipped = new Set(offers.map((offer) => `${offer.copy.repo}\0${offer.copy.name}`));

  // Context files: the session repository's, one at each level from the working directory up to
  // the checkout's root (`levels:`, CONTEXT_FILES); for a Legion pane, the project's instructions
  // file too, and for a dev box the user-level context file Oh My Pi reads. A recorded prompt
  // already holds them as its session had them. `why` says why a user-level file is that one.
  const contextFiles: { repo: string; path: string; why?: string }[] = [];
  if (input.recordedPrompt) {
    evidence.push({
      item: "no context files",
      source:
        "the session's recorded system prompt holds the context files it was given, and the capture sends that prompt",
    });
  } else if (legion) {
    if (checkout.repo === undefined) {
      evidence.push({ item: NO_WORKSPACE, source: checkout.line.source });
    } else {
      contextFiles.push({ repo: checkout.repo, path: "levels:" });
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
  } else if (devbox || ompProfile === "unknown") {
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
        why: `Oh My Pi's user-level context file is CLAUDE.md in Claude Code's config directory, on a dev box the .claude/ of the repository that installs its units: of the repositories of the profile's units (${unitRepos.join(", ")}), ${home} alone holds Claude Code's settings ${CLAUDE_SETTINGS} at ${commits.get(home)}`,
      });
    } else {
      evidence.push({
        item: "left out: Oh My Pi's user-level context file",
        source: `${homes.length === 0 ? "no repository" : `${homes.length} repositories (${homes.join(", ")})`} of the profile's units (${unitRepos.join(", ") || "none"}) ${homes.length === 0 ? "holds" : "hold"} Claude Code's settings ${CLAUDE_SETTINGS} at the session's start, so which is the dev box's Claude Code config directory, whose CLAUDE.md Oh My Pi reads at user level, is not known`,
      });
    }
    if (checkout.repo !== undefined) contextFiles.push({ repo: checkout.repo, path: "levels:" });
  }
  if (isolated && ompProfile !== "unknown" && !input.recordedPrompt && checkout.repo !== undefined)
    contextFiles.push({ repo: checkout.repo, path: "levels:" });

  const profileSkills: ProfileSkill[] = [];
  for (const skill of [...skills.values()].sort((a, b) =>
    listedName(a) < listedName(b) ? -1 : listedName(a) > listedName(b) ? 1 : 0
  )) {
    // A skill at its repository's root (a clone that is one skill) has the path ".".
    profileSkills.push({
      ...skill,
      path: skill.path === "" ? "." : skill.path,
      commit: skill.commit ?? (await commitOf(skill.repo)),
    });
  }
  // A skill whose SKILL.md hides it is installed, so skill:// reads it, and not listed.
  const hidden = new Set<string>();
  for (const skill of profileSkills) {
    const at = treeOf(skill);
    if (at?.hidden.has(repoPath(skill.path)) !== true) continue;
    hidden.add(listedName(skill));
    evidence.push({
      item: `not listed: skill ${listedName(skill)}`,
      source: `${skill.repo} ${skill.path} at ${skill.commit}: ${HIDDEN_RULE}`,
    });
  }
  const profilePlugins: Profile["plugins"] = [];
  for (const plugin of plugins.values()) {
    // The plugin directory of a unit of the worker image is at the image's commit too.
    const commit = plugin.repo === image?.repo ? image.sha : await commitOf(plugin.repo);
    profilePlugins.push({ repo: plugin.repo, commit, path: plugin.path, name: plugin.name });
    evidence.push({
      item: `plugin ${plugin.name} (${plugin.repo} ${plugin.path})`,
      source: plugin.why,
    });
  }
  const profileFiles: Profile["contextFiles"] = [];
  for (const file of contextFiles) {
    // A repository with no commit then is left out above, with why.
    if (absent.has(file.repo)) continue;
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
    if (file.path === "levels:") {
      // The session repository is read at its commit (projectTree), so each level's files are known.
      const tree = projectTree;
      if (tree === undefined) continue;
      const ompLevel = levels.find((level) => tree.ompLevels.has(level));
      let kept = 0;
      // The farthest level first, as Oh My Pi joins them.
      for (const [depth, level] of [...levels.entries()].reverse()) {
        const at = (spec: (typeof CONTEXT_FILES)[number]) =>
          level === "" ? spec.file : `${level}/${spec.file}`;
        const present = CONTEXT_FILES.filter((spec) => {
          if (spec.at === "cwd" && depth !== 0) return false;
          if (spec.at === "omp" && level !== ompLevel) return false;
          const size = tree.contextSizes.get(at(spec));
          return size !== undefined && (size > 0 || spec.empty);
        });
        const [first, ...shadowed] = present;
        if (first === undefined) continue;
        kept += 1;
        const place = `${level === "" ? "the checkout's root" : level}, depth ${depth}`;
        profileFiles.push({ repo: file.repo, commit, path: at(first) });
        evidence.push({
          item: `context file ${file.repo} ${at(first)}`,
          source: `${CONTEXT_SLOT_RULE}: at ${place}, ${file.repo} (the session repository) has ${present.map(at).join(", ")} at ${commit}, and Oh My Pi keeps ${at(first)}, priority ${first.priority} (${first.source})`,
        });
        for (const spec of shadowed) {
          evidence.push({
            item: `left out: ${file.repo} ${at(spec)}`,
            source: `${CONTEXT_SLOT_RULE}: at ${place}, ${at(first)} (priority ${first.priority}) comes before it (priority ${spec.priority}; ${spec.source})`,
          });
        }
      }
      if (kept === 0) {
        evidence.push({
          item: `left out: the context files of ${file.repo}`,
          source: `${CONTEXT_SLOT_RULE}: no level from the working directory up to the checkout's root holds one at ${commit}`,
        });
      }
      continue;
    }
    // The user-level context file: `why` says why it is that one.
    if ((await github.text(file.repo, commit, file.path)) === undefined) {
      evidence.push({
        item: `left out: ${file.repo} ${file.path}`,
        source: `${file.why}, and it has no ${file.path} at ${commit}`,
      });
      continue;
    }
    profileFiles.push({ repo: file.repo, commit, path: file.path, user: true });
    evidence.push({
      item: `context file ${file.repo} ${file.path}`,
      source: `${file.why}, and it has ${file.path} at ${commit}`,
    });
  }
  // A session repository with no commit then is left out above, so the profile names none.
  const sessionRepo =
    checkout.repo === undefined || absent.has(checkout.repo) || notCheckout
      ? undefined
      : checkout.repo;
  return {
    profile: {
      skills: profileSkills,
      plugins: profilePlugins,
      contextFiles: profileFiles,
      sessionRepository:
        sessionRepo === undefined
          ? undefined
          : { repo: sessionRepo, commit: await commitOf(sessionRepo) },
      workingDirectory: sessionRepo === undefined || checkout.rel === "" ? undefined : checkout.rel,
    },
    evidence,
    shipped,
    hidden,
    again,
    fromRecorded: [...fromRecorded].sort(),
  };
}

// ---------------------------------------------------------------------------------------------
// build-cases

const BUILD_CASES_USAGE =
  "usage: build.ts build-cases --config <skill-cuts.json> --key-file <file> --cut <id> --skill <name> --old <dir> [--new <dir>] --sessions <file> --seed <n> --copies <dir> --count <dir> --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]\n       build.ts build-cases --config <skill-cuts.json> --old <dir> --copies <dir> --count <dir> --reprofile <cases.jsonl> --out <new cases.jsonl>";
const BUILD_TASKS_USAGE =
  "usage: build.ts build-tasks --config <skill-cuts.json> --key-file <file> --skill <dir> --store <db>... --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir> --context-session <store>:<hash>] [--max-candidates <n>] [--max-search-minutes <m>]\n       build.ts build-tasks --config <skill-cuts.json> --copies <dir> --count <dir> --reprofile <tasks.jsonl> --profiles-out <new dir> [--context-session <store>:<hash>]\n       build.ts build-tasks --first-read --config <skill-cuts.json> --key-file <file> --skill <dir> --sessions <sessions.jsonl> --seed <n> [--max <n>] --out <tasks.jsonl> [--model <model>] [--profiles-out <dir> --copies <dir> --count <dir>]";

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

/** The skill in `dir` as build-tasks reads it: its SKILL.md's name and description as Oh My Pi
 *  reads them (common.ts's `ompText`, `readFrontmatter` and `ompSkillName`: its frontmatter name,
 *  trimmed, else its directory's name). Refuses a directory with no SKILL.md. */
function taskSkill(dir: string): { name: string; description: string } {
  const skillFile = path.join(dir, "SKILL.md");
  mustBeFile(skillFile, "--skill's SKILL.md");
  const fields = readFrontmatter(ompText(readFileSync(skillFile)));
  return {
    name: ompSkillName(fields, path.basename(path.resolve(dir))),
    description: typeof fields.description === "string" ? fields.description : "",
  };
}

/** The gateway of a builder's readers, which records each answer's usage with `spendRecorder`
 *  (none when undefined), starts no call once its spend reaches `budgetUsd`, and takes
 *  `concurrency` calls at once. */
function readerGateway(
  config: SkillCutsConfig,
  keyFile: string,
  spendRecorder: SpendRecorder | undefined,
  budgetUsd?: number,
  concurrency = 1
): Gateway {
  return createGateway({
    baseUrl: config.gatewayBaseUrl,
    keyFile,
    concurrency,
    timeoutMs: READER_TIMEOUT_MS,
    ...(budgetUsd === undefined ? {} : { budgetUsd }),
    spendRecorder: spendRecorder ?? null,
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

/** What one session gave: a case, with what its listing measure is made from and what the store
 *  gave of its subagents for it (read before its profile was built); or a drop with its reason. */
type Outcome =
  | {
      readonly case: Case;
      readonly step: number;
      readonly listing: ListingInput;
      readonly read: SubagentRead;
    }
  | { readonly drop: DropReason; readonly detail: string };

/** What one session's build reads beside the session. */
interface CaseRun {
  readonly config: SkillCutsConfig;
  readonly cut: { readonly id: string; readonly repo: string };
  readonly skill: string;
  readonly old: string;
  /** The skill's directory at the cut's head (`--new`): the new arm's text, whose files a case's
   *  reads by path are checked against too (`pathFormIn`); undefined for a build given none (no
   *  cut text yet), which checks the old text alone. */
  readonly new: string | undefined;
  /** The skill's base files (`--old`). */
  readonly files: readonly { readonly path: string; readonly text: string }[];
  readonly copies: Copies;
  readonly github: GithubReads;
  readonly gateway: Gateway;
  /** attribute.ts's placing of reads, for the build's `--count` and `--copies`. */
  readonly placer: Placer;
  /** The ids (attribute.ts's `modelId`) of the configuration's `servedModels`. */
  readonly servedIds: ReadonlySet<string>;
  /** Runs one read of the store when the store's other reads of the build have ended: the store
   *  is read one query at a time, whichever sessions are open. */
  readStore<T>(read: () => Promise<T>): Promise<T>;
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

/** The Oh My Pi version a session's file records it ran: the `runtime.version` of its first
 *  `model_change` line, as session-head.sql reads it (POOL_VERSION_RULE); undefined where that
 *  line records none, or the file holds no such line. */
function runtimeOf(t: Transcript): string | undefined {
  const runtime = t.entries.find((entry) => entry.type === "model_change")?.runtime;
  return isObject(runtime) && typeof runtime.version === "string" ? runtime.version : undefined;
}

/** The schema a written task profile must pass: the one load reads it with (cases.ts's Profile
 *  and the context of a case). */
const WrittenProfile = Profile.extend({ context: Case.shape.context });

/** The skills of a task profile a build keeps, whatever else it holds: a written profile holds a
 *  `context` beside cases.ts's Profile, and the listing its measure sets it against
 *  (`withRecordedListing`), which the strict Profile refuses. */
const KeptSkills = z.looseObject({ skills: Profile.shape.skills });

/** A rebuilt task profile its readers would refuse (WrittenProfile), which is never written. */
class ProfileRefused extends Error {}

/** A load test's profile of one session (load.ts's LoadProfile: cases.ts's Profile, the context,
 *  and, once `withRecordedListing` adds it, the listing its measure sets it against), the
 *  evidence for each part, the secrets taken out of it, whether a load test of the skill in it
 *  lists the skill (`listsSkill`), the session's store and path hash, by which its subagents are
 *  asked for, and the profile's copy of the tested skill's source (`testedCopy`: the entry listed
 *  under its name, which the profile's skills leave out), where it had one. */
export interface SessionProfile {
  readonly profile: Profile & {
    readonly context: string;
    readonly recordedListing?: RecordedListing;
  };
  readonly evidence: Evidence[];
  readonly secretsRemoved: number;
  /** What its listing measure is made from (`listingMeasure`). */
  readonly listing: ListingInput;
  /** What the store gave of its subagents for that measure, read before its profile was built,
   *  whose listing decided its pools (POOL_LISTING_RULE). */
  readonly read: SubagentRead;
  readonly listed: boolean;
  readonly store: string;
  readonly pathHash: string;
  readonly testedCopy?: ProfileSkill;
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
  return skills.some((each) => listedName(each) === skill);
}

/** How `sessionProfile` rebuilds a session's profile: as a context session (`asContext`, see
 *  there), with what its caller read of its subagents (`read`: a context session's, read first to
 *  find the build it is read at); and, for a subagent, with its digest-times.sql row read by
 *  `digestRow` (by its path hash; undefined when the store holds no file of it) where a caller
 *  reads the rows of many sessions in one query, else by one query of its own. */
interface SessionProfileOptions {
  readonly asContext?:
    | { readonly spawned: { readonly pathHash: string; readonly at: string } }
    | { readonly unspawned: string };
  readonly read?: SubagentRead;
  readonly digestRow?: (pathHash: string) => Promise<DigestRow | undefined>;
}

/**
 * The load test's profile of the session whose transcript is `t` (read from the file
 * `transcriptPath` of `store`; its caller parsed it, `readTranscript`), rebuilt as build-cases
 * rebuilds a case's: its context (`sessionContext`), and `caseProfile` from every read of its
 * branch, so the units its host had, at their commits then, their plugins and the context files; a
 * subagent's recorded system prompt (its secrets taken out) and tools. A subagent's is read at its
 * root's last prompt build before its start (PARENT_LOAD_RULE); a top-level session's at its first
 * build (TOP_BUILD_RULE: a task runs as its first message). A context session (`asContext`) has no
 * step: it is read at the build its subagent `spawned` (the one its listing measure uses: its path
 * hash and start) was spawned under, or, when no subagent names a build, at its first build, and
 * `unspawned` says why none does. Each copy of the tested skill `skill`'s source (cases.ts's
 * `testedCopy`) is left out of the skills, since the load test installs it; a copy of another
 * source that shares the name stays, under its namespace. The evidence says where the session's
 * host had the tested source at another install path too (`held once`). Throws, naming why, when
 * the session gives none.
 */
async function sessionProfile(
  run: ProfileRun,
  store: StoreContext,
  transcriptPath: string,
  t: Transcript,
  skill: string,
  options: SessionProfileOptions = {}
): Promise<SessionProfile> {
  const { asContext } = options;
  const header = sessionHeader(t);
  if ("drop" in header) throw new Error(`${header.drop}: ${header.detail}`);
  const rebuilt = sessionContext(store, t, transcriptPath);
  if ("drop" in rebuilt) throw new Error(`${rebuilt.drop}: ${rebuilt.detail}`);
  const { context, project, evidence, init } = rebuilt;
  // The window a context session's subagents were asked for: every time (rebuiltContext).
  const anyTime = { from: null, to: null, words: "at any time" };
  let readTime: ReadTime;
  let build: BuildWindow | undefined;
  const hash = pathHash(transcriptPath);
  if (init !== undefined) {
    const row =
      options.digestRow === undefined
        ? (await digestRows(run.config.psqlCommand, store.name, [hash])).get(hash)
        : await options.digestRow(hash);
    readTime = parentLoad(row, header.sessionStart, store.name);
  } else if (asContext === undefined) {
    ({ read: readTime, window: build } = topBuild(
      digestLines(t),
      header.sessionStart,
      -1,
      "a task runs as its session's first message, under the session's first prompt build"
    ));
  } else if ("unspawned" in asContext) {
    ({ read: readTime } = topBuild(
      digestLines(t),
      header.sessionStart,
      -1,
      `a context session has no step, and no subagent names the build it is read at: ${asContext.unspawned}`
    ));
    build = anyTime;
  } else {
    const { spawned } = asContext;
    const lines = digestLines(t);
    const index = lines.findLastIndex((line) => Date.parse(line.at) < Date.parse(spawned.at));
    ({ read: readTime } = topBuild(
      lines,
      header.sessionStart,
      index,
      `its subagent ${spawned.pathHash}, the earliest whose recorded prompt holds a skill listing and the one its listing measure uses, started at ${spawned.at}, under the build of the last system-prompt-digest line before then (${index < 0 ? "none" : lines[index]?.at})`,
      "the prompt build its measured subagent was spawned under"
    ));
    build = {
      ...anyTime,
      words:
        "at any time, the profile being read at the build the earliest of them was spawned under",
    };
  }
  // The listing its measure sets it against decides its pools (POOL_LISTING_RULE): a top-level
  // session's subagent is read from the store first, one subagents.sql query, unless its caller
  // read it already.
  const base: ListingBase = {
    recorded: init?.systemPrompt,
    ...(build === undefined ? {} : { build }),
  };
  const read =
    options.read ?? (await readSubagents(run.config.psqlCommand, store.name, hash, base));
  const recordedListing = decidingListing(base, read);
  const messages = t.items.map((item) => item.slim);
  const profiled = await caseProfile(run, {
    store: store.name,
    context: store.context,
    ompProfile: sessionProfileName(transcriptPath),
    project,
    sessionStart: header.sessionStart,
    cwd: header.cwd,
    recordedPrompt: init !== undefined,
    ...(recordedListing === undefined ? {} : { recordedListing }),
    runtime: runtimeOf(t),
    reads: shownReads(messages),
    absoluteReads: absoluteReads(messages),
    readTime,
  });
  const secrets = scrubbed({ systemPrompt: init?.systemPrompt });
  const recorded =
    init === undefined || secrets.value.systemPrompt === undefined
      ? {}
      : { systemPrompt: secrets.value.systemPrompt, tools: init.tools };
  // Each copy of the tested skill's source, the load test installs (cases.ts's `testedCopy`); a
  // copy of another source that shares its name stays.
  const tested = testedCopy(profiled.profile.skills, skill);
  const source = testedSource(profiled.profile.skills, skill);
  const profile = {
    ...profiled.profile,
    ...recorded,
    skills: profiled.profile.skills.filter((each) => !tested(each)),
    context,
  };
  // A profile the readers would refuse is never written (load reads it with cases.ts's Profile
  // and a context): the task fails with why.
  const checked = WrittenProfile.safeParse(profile);
  if (!checked.success) {
    throw new ProfileRefused(
      `its rebuilt profile is one load refuses (cases.ts's Profile and a context): ${schemaFaults(checked.error)}`
    );
  }
  return {
    profile,
    evidence: [
      ...evidence,
      ...profiled.evidence,
      // The tested source at another install path: a host that had it there at another commit
      // listed it a second time under a namespace, which the profile does not hold.
      ...profiled.again
        .filter(({ copy }) => tested(copy))
        .map(({ copy, how, namespace }) => ({
          item: `held once: skill ${skill} (${copy.repo} ${copy.path})`,
          source: `${how} too, the same copy; a host that had it there at another commit than the first install's lists that one as ${namespace}/${skill} (NAMESPACE_RULE), and the profile, which reads ${copy.repo} at one commit, holds the tested skill once, so a recorded ${namespace}/${skill} is counted missing by the listing measure`,
        })),
    ],
    store: store.name,
    pathHash: hash,
    secretsRemoved: secrets.removed,
    listed: listsSkill(recorded, profiled.profile.skills, skill),
    listing: {
      listed: listedNames(profiled),
      fromRecorded: profiled.fromRecorded,
      recorded: init?.systemPrompt,
      sessionDigest: t.branch.map(promptDigest).find((digest) => digest !== undefined) ?? null,
      ...(build === undefined ? {} : { build }),
    },
    read,
    // The copy listed under the tested skill's name: the recorded listing's line of it stands for
    // that copy's text (`withRecordedListing`).
    ...(source === undefined ? {} : { testedCopy: source }),
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
  // The session's reads of the skill: by a skill:// URL, and by its path (count.sql's rule). A case
  // is built from a successful skill:// read, or from a successful whole read by path that
  // attribute.ts's rules place at the cut's copy (a source); the step comes after the first of
  // them.
  const reads = skillReads(allMessages, run.skill);
  const ompProfile = sessionProfileName(transcriptPath);
  const placeRead = (copyPath: string) =>
    run.placer.place(store.name, copyPath, run.skill, ompProfile);
  // A read by path relative to the session's working directory, or under `~`, resolves against
  // its header's cwd and the home its transcript's path names.
  const dirs = sessionDirs(header.cwd, transcriptPath);
  const byPath = pathReads(allMessages, run.skill, placeRead, run.cut.repo, dirs);
  const sourceCopies = [
    ...new Set(byPath.flatMap((read) => (read.ok && read.dir !== undefined ? [read.dir] : []))),
  ].map((dir): CopyPlace => ({ dir, ...dirs }));
  const firstUrl = reads.find((read) => read.ok);
  const firstPath = byPath.find((read) => read.ok && read.kind === "source");
  const first =
    firstPath === undefined || (firstUrl !== undefined && firstUrl.index < firstPath.index)
      ? firstUrl
      : firstPath;
  if (first === undefined) {
    // A session whose only successful reads of the skill are reads by path that no case is built
    // from is dropped by the first of their kinds, in EXCLUDED_KINDS' order.
    const excluded = byPath.filter((read) => read.ok);
    const kind = EXCLUDED_KINDS.find((each) => excluded.some((read) => read.kind === each));
    if (kind !== undefined) {
      return drop(
        kind,
        `the session reads ${run.skill} by path only so: ${excluded.map((read) => excludedRead(read)).join("; ")}`
      );
    }
    return drop(
      "no-read",
      `${reads.length} read results of skill://${run.skill} files and ${byPath.length} of reads of its files by path, none successful`
    );
  }
  const firstAt = t.items[first.index]?.at ?? 0;
  record.model = lastBefore(t.branch, firstAt, modelChange);
  // The copy a skill:// read read, placed by attribute.ts's own rules from the same count and
  // copies output as the sessions file, so the builder keeps what attribute.ts counted for the
  // cut's copy: a read in a checkout or one the store's context places, as well as one the prefix
  // table maps. A read by path is a source only when they place it at the cut's copy.
  if (first === firstUrl) {
    const placed = placeRead(firstUrl.header ?? "");
    const [placedRepository = "", placedPath = ""] = (placed.copy ?? "").split("\t");
    if (placed.copy === undefined) {
      return drop("not-cut-copy", `no rule places ${firstUrl.header} at a copy: ${placed.outcome}`);
    }
    if (placedRepository !== run.cut.repo) {
      return drop(
        "not-cut-copy",
        `the ${placed.rule} rule places ${firstUrl.header} at ${placedRepository} ${placedPath}${placed.outcome === undefined ? "" : ` (${placed.outcome})`}`
      );
    }
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
  // reads of the skill's files held (by skill://, or whole by path from a source's copy), among
  // the messages it reads. A session with none gives no case, and no reader reads it.
  const pathFileReads = copiesReads(allMessages, sourceCopies);
  const readsInReach = [...reads, ...pathFileReads].filter(
    (read) => read.ok && read.index < readable.length
  );
  const offered = offeredFiles(run.files, readsInReach, t.message);
  if (offered.length === 0) {
    const read = [...new Set(readsInReach.map((each) => each.file))];
    return drop(
      "no-common-line",
      `the session's successful reads of ${read.join(", ")} hold no whole line of the base text`
    );
  }

  // The profile of the reads the reader can reach, before any reader reads the session. The
  // case's profile is built from the reads before its step, which the reader has not found yet;
  // those are among the reads the reader can reach, and fewer reads install fewer units. A
  // subagent's repositories are read at its root's last prompt build before its start
  // (PARENT_LOAD_RULE); a top-level session's at the build its step ran under (TOP_BUILD_RULE),
  // which is one of the builds from the skill's read to the last message the reader can read, or
  // the one whose line comes next (the step's own), so the profile is built at each of those. When
  // no unit of the reads it can reach ships the skill of the cut's repository at any of them, no
  // step's profile installs it, and the session is dropped as the step's profile check would drop
  // it. A GitHub read that fails here is the step's read too.
  const lines = init === undefined ? digestLines(t) : [];
  let reach: ReadTime[];
  if (init !== undefined) {
    try {
      const hash = pathHash(transcriptPath);
      const rows = await run.readStore(() =>
        digestRows(run.config.psqlCommand, store.name, [hash])
      );
      reach = [parentLoad(rows.get(hash), sessionStart, store.name)];
    } catch (error) {
      return drop("profile-failed", messageFor(error));
    }
  } else {
    const readAt = entryTime(t.branch[firstAt] ?? {});
    const endAt = entryTime(t.branch[readable.at(-1)?.at ?? firstAt] ?? {});
    const from = Math.max(
      lines.findLastIndex((line) => Date.parse(line.at) <= readAt),
      0
    );
    const to = Math.min(
      lines.findLastIndex((line) => Date.parse(line.at) <= endAt) + 1,
      lines.length - 1
    );
    // Each build from the read's (the first for a read before every line) to the reach's last.
    const builds = [from];
    for (let index = from + 1; index <= to; index += 1) builds.push(index);
    reach = builds.map(
      (index) =>
        topBuild(
          lines,
          sessionStart,
          index,
          "a build a step in the case reader's reach can run under"
        ).read
    );
  }
  const profileInput = {
    store: store.name,
    context: store.context,
    ompProfile,
    project,
    sessionStart,
    cwd: header.cwd,
    recordedPrompt: init !== undefined,
    runtime: runtimeOf(t),
  };
  // A subagent's pools are decided by its own recorded listing (POOL_LISTING_RULE); a top-level
  // session's by the listing of a subagent spawned under its step's build, not known yet, so this
  // check installs every pool, which is at least what any step's profile installs.
  const reachPools =
    init === undefined
      ? { poolsUndecided: true as const }
      : {
          recordedListing: decidingListing(
            { recorded: init.systemPrompt },
            unasked({ recorded: init.systemPrompt })
          ),
        };
  const reachableMessages = readable.map((item) => item.slim);
  const reachableReads = profileReads(reachableMessages, run.skill, sourceCopies);
  const reachableAbsolute = absoluteReads(reachableMessages);
  let ships = false;
  for (const readTime of reach) {
    try {
      const reachable = await caseProfile(run, {
        ...profileInput,
        ...reachPools,
        readTime,
        reads: reachableReads,
        absoluteReads: reachableAbsolute,
      });
      ships = reachable.shipped.has(`${run.cut.repo}\0${run.skill}`);
    } catch (error) {
      return drop("profile-failed", messageFor(error));
    }
    if (ships) break;
  }
  if (!ships) {
    const times = reach.map((each) => each.at ?? "the session's start");
    return drop(
      "not-cut-copy",
      notInstalled(
        run,
        init !== undefined || reach.every((each) => each.at === undefined)
          ? undefined
          : `${times.join(" or ")}, the prompt builds a step in the case reader's reach can run under`
      )
    );
  }
  const hidden = new Set([
    ...skillReadResults(allMessages, run.skill),
    ...pathFileReads.map((read) => read.index),
  ]);
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
    // A session whose work at the read is on the skill's own text gives no case of the skill's
    // behaviour in any window: its next step works on the text and does not follow it.
    if ("material" in given) {
      return drop(
        "skill-as-material",
        `the case reader, in window ${windows} (messages ${windowMessages[0]?.index} to ${windowMessages.at(-1)?.index}): ${given.material.why}`
      );
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
  // The case's reads of the skill's files: each skill:// read, and, when a source before the step
  // read the cut's copy whole by path, each whole read by path of a file of that copy (the first
  // such source's resolved directory, with the session's cwd and home: the case's `pathCopy`).
  // Each other successful read by path of the skill is answered as recorded in every arm, and the
  // case's evidence names it (`excluded reads`).
  const caseReads = skillReads(messages, run.skill);
  const casePaths = pathReads(messages, run.skill, placeRead, run.cut.repo, dirs);
  const sourceDir = casePaths.find((read) => read.ok && read.kind === "source")?.dir;
  const pathCopy: CopyPlace | undefined =
    sourceDir === undefined ? undefined : { dir: sourceDir, ...dirs };
  const fileReads = [
    ...caseReads,
    ...(pathCopy === undefined ? [] : copyReads(messages, pathCopy)),
  ].sort((a, b) => a.index - b.index);
  const answered = new Set(fileReads.map((read) => read.index));
  const excluded = casePaths.filter((read) => read.ok && !answered.has(read.index));
  const installedDirs = caseReads.flatMap((read) =>
    read.header?.endsWith(`/${read.file}`) ? [read.header.slice(0, -(read.file.length + 1))] : []
  );
  const kept = new Set(
    [...fileReads, ...excluded].map((read) => messages[read.index]?.toolCallId ?? "")
  );
  const unheld = readByPath(messages, run.skill, installedDirs, kept);
  if (unheld !== undefined) return drop("read-by-path", unheld);
  // A case whose read by path the arms do not answer in Oh My Pi's form would fail every
  // `behaviour` run of it: the old text's, and the new text's when `--new` is given (cli.ts's
  // screen drops a case on the new text's otherwise).
  const formFault = pathFormIn(
    fileReads.flatMap((read) => (read.path === undefined ? [] : [read.file])),
    run
  );
  if (formFault !== undefined) {
    return drop("path-form", `in the ${formFault.arm} text ${formFault.dir}: ${formFault.fault}`);
  }

  const file = run.files.find((each) => each.path === answer.line.file);
  const wanted = answer.line.text.trim();
  const baseLines = file === undefined ? [] : textLines(file.text);
  const lineNumber = baseLines.findIndex((line) => line.trim() === wanted) + 1;
  if (file === undefined || lineNumber === 0) {
    return drop("line-not-in-base", `${answer.line.file}: ${JSON.stringify(answer.line.text)}`);
  }
  const held = fileReads.some(
    (read) =>
      read.ok &&
      read.file === file.path &&
      textLines(readText(read, messages[read.index])).some((line) => line.trim() === wanted)
  );
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

  const digest = lastBefore(t.branch, stepItem.at, promptDigest);
  // The profile: from the reads before the step, read at the root's last prompt build before a
  // subagent's start (PARENT_LOAD_RULE), or at the build a top-level session's step ran under
  // (TOP_BUILD_RULE). Its GitHub reads were made for the reads the reader could reach, so they
  // are made once.
  const step =
    init === undefined
      ? stepBuild(lines, sessionStart, messageTime(t.message(stepIndex)), digest ?? null)
      : undefined;
  const readTime = step?.read ?? reach[0];
  // The listing its measure sets it against decides its pools (POOL_LISTING_RULE): a top-level
  // session's is that of its earliest subagent spawned under its step's build, read from the store
  // now, one query at a time with the pulls; the measure takes this read too.
  const base: ListingBase = {
    recorded: init?.systemPrompt,
    ...(step === undefined ? {} : { build: step.window }),
  };
  let read: SubagentRead;
  let profiled: ProfileResult;
  try {
    read = await readSubagents(
      run.config.psqlCommand,
      store.name,
      pathHash(transcriptPath),
      base,
      run.readStore
    );
    const recordedListing = decidingListing(base, read);
    profiled = await caseProfile(run, {
      ...profileInput,
      ...(recordedListing === undefined ? {} : { recordedListing }),
      readTime,
      reads: profileReads(messages, run.skill, pathCopy === undefined ? [] : [pathCopy]),
      absoluteReads: absoluteReads(messages),
    });
  } catch (error) {
    return drop("profile-failed", messageFor(error));
  }
  if (
    !profiled.profile.skills.some(
      (each) =>
        each.name === run.skill && each.repo === run.cut.repo && each.namespace === undefined
    )
  ) {
    return drop(
      "not-cut-copy",
      notInstalled(
        run,
        readTime?.at === undefined || step === undefined
          ? undefined
          : `${readTime.words} ${readTime.at}`
      )
    );
  }

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
  const skillReadIndices = fileReads.map((each) => each.index);

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
    ...(pathCopy === undefined ? {} : { pathCopy }),
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
      ...(excluded.length === 0
        ? []
        : [
            {
              item: EXCLUDED_READS,
              source: `each answered as recorded in every arm: ${excluded.map((read) => excludedRead(read, pathCopy)).join("; ")}`,
            },
          ]),
    ],
  });
  return {
    case: built,
    step: stepIndex,
    listing: {
      listed: listedNames(profiled),
      fromRecorded: profiled.fromRecorded,
      recorded: init?.systemPrompt,
      sessionDigest: digest ?? null,
      ...(step === undefined ? {} : { build: step.window }),
    },
    read,
  };
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
    placing = placer(
      count.outputs,
      r4,
      count.profiles,
      new Set(
        config.stores.filter((store) => store.context === "devbox").map((store) => store.name)
      )
    );
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
/** The item of a case's evidence that names each successful read by path of the skill before its
 *  step that the arms answer as recorded (`excludedRead`); it comes after the audit's. */
export const EXCLUDED_READS = "excluded reads";
/** The item of the profile's evidence for a Legion session whose working directory is no
 *  workspace, so its repository's context files are left out. */
const NO_WORKSPACE = "left out: the session repository's context files";
/** The item of a reprofiled case's evidence when its store holds its session no longer. */
const NO_SESSION_HEAD = "no session head";

/**
 * `build-cases --reprofile`: the cases of a case file, each with its profile built again from the
 * case and its session's head, with no transcript pulled and no reader call. A case holds what the
 * profile is built from but the session's working directory: its store, its messages (their
 * `[Skill file: …]` lines), its start and whether it carries a recorded prompt. The working
 * directory is the header's `cwd` in the session's head, read from its store (capture.ts's
 * `sessionHeads`: session-head.sql through the store command, one query per store, each head saved
 * under `<out>.sessions/`). For a case whose session the store no longer holds, the evidence says
 * so (`no session head`), and the session repository is the one the old case's evidence names
 * (`session repository <repo>`, or `no session repository`), that item kept as it is; a case built
 * before that item was written is taken to be in the workspace of the repository whose AGENTS.md
 * the old profile holds, and one whose old profile holds none keeps the old evidence of why. Only
 * `profile` (its skills, plugins and context files) and the profile's part of `evidence` change.
 * A case whose profile cannot be built, or no longer installs its skill, is left out and named,
 * and the command fails; a read of the heads that fails fails it too.
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
        `--reprofile pulls no session and calls no reader, so it takes no --${flag}`
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
    github: githubReads(io, config, copies.branches, "build-cases"),
    placer: placing,
  };
  // Each case's session head, for its working directory.
  const heads = new Map<string, Map<string, SavedHead>>();
  const transcriptPaths = new Map<string, Map<string, string>>();
  for (const name of new Set(cases.map((c) => c.source.store))) {
    if (!config.stores.some((each) => each.name === name)) continue;
    const hashes = cases.filter((c) => c.source.store === name).map((c) => c.source.pathHash);
    heads.set(name, await sessionHeads(config.psqlCommand, name, hashes, `${given.out}.sessions`));
    const resolved = await resolvePaths(config.psqlCommand, name, hashes, sessionPathsSql);
    transcriptPaths.set(name, resolved.paths);
  }
  // When each case's repositories are read: a subagent's at its root's last prompt build before
  // its start (PARENT_LOAD_RULE), a top-level session's at the build its step ran under
  // (TOP_BUILD_RULE), each from the digest lines of its root, a top-level session being its own:
  // one digest-times.sql query a store, for the subagents' cases and the top-level sessions the
  // store holds.
  const reads = new Map<string, { readonly read: ReadTime; readonly window?: BuildWindow }>();
  for (const [name, saved] of heads) {
    const ofStore = cases.filter((c) => c.source.store === name);
    const held = (c: Case) => saved.get(c.source.pathHash)?.head?.held === true;
    const rows = await digestRows(
      config.psqlCommand,
      name,
      ofStore
        .filter((c) => c.profile.systemPrompt !== undefined || held(c))
        .map((c) => c.source.pathHash)
    );
    for (const c of ofStore) {
      const row = rows.get(c.source.pathHash);
      reads.set(
        c.id,
        c.profile.systemPrompt !== undefined
          ? { read: parentLoad(row, c.sessionStart, name) }
          : stepBuild(
              row === undefined
                ? `the store ${name} holds no file of the session now`
                : row.digests,
              c.sessionStart,
              messageTime(c.realStep),
              c.sessionDigest
            )
      );
    }
  }
  // A case whose session records no prompt is measured against the earliest subagent of it
  // spawned under the build its profile is read at whose recorded prompt holds a listing: one
  // subagents.sql query a store, for the sessions the store holds.
  const subagents = new Map<string, Map<string, SubagentsFound>>();
  for (const [name, saved] of heads) {
    const sessions = cases.flatMap((c) => {
      const window = reads.get(c.id)?.window;
      return c.source.store === name &&
        c.profile.systemPrompt === undefined &&
        saved.get(c.source.pathHash)?.head?.held === true &&
        window !== undefined &&
        !("unknown" in window)
        ? [{ hash: c.source.pathHash, from: window.from, to: window.to }]
        : [];
    });
    subagents.set(name, await subagentListings(config.psqlCommand, name, sessions));
  }
  const rebuilt: Case[] = [];
  const records: Record<string, unknown>[] = [];
  const measures: (ListingMeasure | undefined)[] = [];
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
    const head = heads.get(store.name)?.get(c.source.pathHash)?.head;
    const held = head?.held === true;
    const old = c.evidence ?? [];
    const decided = old.find(
      (each) =>
        each.item === NO_SESSION_REPOSITORY_ITEM || each.item.startsWith(SESSION_REPOSITORY_ITEM)
    );
    const legion = store.context.startsWith("legion:");
    const agents = legion
      ? c.profile.contextFiles.find((file) => file.path === "AGENTS.md")
      : undefined;
    // Without the session's head, the old rule: the session repository the old evidence names,
    // else the workspace of the repository whose AGENTS.md the old profile holds.
    const headless: Evidence[] = held
      ? []
      : [
          {
            item: NO_SESSION_HEAD,
            source: `the store ${store.name} holds no file of the session now (session-head.sql), so its working directory is not known: ${
              decided !== undefined
                ? "the session repository is the one the old evidence names"
                : agents !== undefined
                  ? `it is taken as the workspace of ${agents.repo}, whose AGENTS.md the old profile holds`
                  : "the old evidence names no session repository"
            }`,
          },
        ];
    const reading = reads.get(c.id);
    const transcriptPath = transcriptPaths.get(store.name)?.get(c.source.pathHash);
    // The Oh My Pi profile: its transcript's path, else (the store holds the session no longer)
    // the one its case saved, else, for a case built before cases saved one, the one the store
    // gave such a case then: the Legion worker's `legion` in a Legion store, `default` in a
    // devbox store.
    const ompProfile =
      transcriptPath !== undefined
        ? sessionProfileName(transcriptPath)
        : (old.find((each) => each.item === OMP_PROFILE_ITEM)?.source ??
          (legion ? "legion" : "default"));
    // The listing its measure sets it against, which decides its pools (POOL_LISTING_RULE): its
    // own recorded prompt's, else that of its subagent the store's query above found. Every case
    // of a store the configuration names has its reading, a window with no recorded prompt and
    // none with one.
    const base: ListingBase = {
      recorded: c.profile.systemPrompt,
      ...(reading?.window === undefined ? {} : { build: reading.window }),
    };
    const between = betweenOf(base);
    const read: SubagentRead = !held
      ? {
          unread:
            "the store holds the session no longer (session-head.sql), so no subagent of it is read",
        }
      : between === undefined
        ? unasked(base)
        : { found: subagents.get(store.name)?.get(betweenKey(c.source.pathHash, between)) };
    const recordedListing = decidingListing(base, read);
    let profiled: ProfileResult;
    try {
      profiled = await caseProfile(run, {
        store: store.name,
        context: store.context,
        ompProfile,
        project: legion ? store.context.slice("legion:".length) : undefined,
        reads: profileReads(c.messages, c.skill, c.pathCopy === undefined ? [] : [c.pathCopy]),
        absoluteReads: absoluteReads(c.messages),
        sessionStart: c.sessionStart,
        cwd: held
          ? (head?.cwd ?? undefined)
          : agents === undefined
            ? undefined
            : `/legion/workspaces/${agents.repo}/`,
        recordedPrompt: c.profile.systemPrompt !== undefined,
        ...(recordedListing === undefined ? {} : { recordedListing }),
        runtime: head?.runtime ?? undefined,
        readTime: reading?.read,
        checkout:
          held || decided === undefined
            ? undefined
            : {
                repo: decided.item.startsWith(SESSION_REPOSITORY_ITEM)
                  ? decided.item.slice(SESSION_REPOSITORY_ITEM.length)
                  : undefined,
                rel: c.profile.workingDirectory ?? "",
                line: decided,
              },
      });
    } catch (error) {
      leave(`${DROP_REASONS["profile-failed"]}: ${messageFor(error)}`);
      continue;
    }
    if (
      !profiled.profile.skills.some(
        (each) => each.name === c.skill && each.repo === cut.repo && each.namespace === undefined
      )
    ) {
      leave(
        notInstalled(
          { skill: c.skill, cut },
          reading?.read.at === undefined || c.profile.systemPrompt !== undefined
            ? undefined
            : `${reading.read.words} ${reading.read.at}`
        )
      );
      continue;
    }
    const profileStart = old.findIndex((each) => !CONTEXT_EVIDENCE.test(each.item));
    // Without the session's head or the old profile's AGENTS.md the working directory is not
    // known, so the old evidence of why it is left out stands in for the new one's.
    const why =
      !held && agents === undefined ? old.filter((each) => /AGENTS\.md$/.test(each.item)) : [];
    const listing: ListingInput = {
      ...base,
      listed: listedNames(profiled),
      fromRecorded: profiled.fromRecorded,
      sessionDigest: c.sessionDigest,
    };
    const evidence = [
      ...old.slice(0, profileStart < 0 ? old.length : profileStart),
      ...headless,
      ...profiled.evidence.flatMap((each) =>
        each.item === NO_WORKSPACE && why.length > 0 ? why : [each]
      ),
      listingMeasure(listing, read),
      ...old.filter((each) => each.item.startsWith(AUDIT_EVIDENCE) || each.item === EXCLUDED_READS),
    ];
    const built = Case.parse({ ...c, profile: { ...c.profile, ...profiled.profile }, evidence });
    rebuilt.push(built);
    const names = (profile: Profile) => profile.skills.map((skill) => skill.name);
    const before = names(c.profile);
    const after = names(built.profile);
    const measure = measureIn(built.evidence);
    measures.push(measure);
    records.push({
      id: c.id,
      skills: after.length,
      sessionHead: held ? "read" : "not held",
      added: after.filter((name) => !before.includes(name)),
      removed: before.filter((name) => !after.includes(name)),
      plugins: built.profile.plugins.map((plugin) => plugin.name),
      contextFiles: built.profile.contextFiles.map((file) => `${file.repo} ${file.path}`),
      listing: measure ?? null,
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
      githubRetries: run.github.retries.record(),
      listing: listingCounts(measures),
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
 *  for a reader of the summary. `new` is every file of `--new`, or null for a run given none; it
 *  is absent from a summary written before build-cases took `--new`, so a build does not go on
 *  from that one. A symbolic link in `--new` counts as the file it reaches, by that file's bytes,
 *  since the build reads `--new` through its links (`pathFormIn`'s `statSync` and `readFileSync`
 *  follow them; the link's own text is read by nothing here); a link that reaches nothing counts
 *  as a missing file, and a directory a link reaches is listed by its files (`readdirSync` goes
 *  through it). */
const CasesInputs = z.strictObject({
  cut: z.string(),
  skill: z.string(),
  seed: z.string(),
  old: Digested,
  new: Digested.nullable().optional(),
  sessions: Digested,
  copies: Digested,
  count: Digested,
});
type CasesInputs = z.infer<typeof CasesInputs>;

/** The files of the copies output that build-cases reads (readCopiesAndPlacer, and attribute.ts's
 *  readR4). */
const COPIES_READ = [
  "former-stages.tsv",
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
      if (file === "roots.txt" || /^\d{4}-\d{2}-\d{2}(T\d{2})?\.txt$/.test(file))
        files.push(`${store}/${file}`);
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
  if (was.new === undefined || was.new?.sha256 !== now.new?.sha256) {
    const shown = (given: Digested | null | undefined) =>
      given === undefined
        ? "not recorded (a build-cases run from before --new)"
        : given === null
          ? "not given"
          : `${given.path} (sha256 ${given.sha256.slice(0, 12)})`;
    differ.push(`--new was ${shown(was.new)}, now ${shown(now.new)}`);
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
  /** Absent from a summary written before build-cases counted its GitHub retries. */
  githubRetries: z.record(z.string(), z.number()).optional(),
});

/** One session an earlier run decided: its summary record and, for a case, the case's summary
 *  record (the case itself stays in the cases file). */
interface DecidedSession {
  readonly record: SessionRecord;
  readonly caseRecord?: Record<string, unknown>;
}

/** What an earlier run of the same inputs decided, by `<store>\0<hash>`, and what it spent,
 *  queried, and sent again of its GitHub reads. */
interface PriorBuild {
  readonly decided: ReadonlyMap<string, DecidedSession>;
  readonly spend: Spend;
  readonly sessionQueries: number;
  readonly githubRetries: Readonly<Record<string, number>>;
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
  return {
    decided,
    spend: summary.readerSpend,
    sessionQueries: summary.sessionQueries,
    githubRetries: summary.githubRetries ?? {},
  };
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
      new: { type: "string" },
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
  // No --new: a build before the cut has a text checks the old text alone.
  const newDir = values.new;
  if (newDir !== undefined && !statSync(newDir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Refused(`--new ${newDir} is not a directory`);
  }
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
    new:
      newDir === undefined
        ? null
        : digestOf(
            newDir,
            readdirSync(newDir, { recursive: true, withFileTypes: true })
              .filter(
                (entry) =>
                  entry.isFile() ||
                  (entry.isSymbolicLink() &&
                    !statSync(path.join(entry.parentPath, entry.name), {
                      throwIfNoEntry: false,
                    })?.isDirectory())
              )
              .map((entry) => path.relative(newDir, path.join(entry.parentPath, entry.name)))
              .sort()
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
  // The spool, held before anything at --out is read or written: a second run of this --out is
  // refused while this one runs.
  const spools = spoolDir(given.out, io);
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
    new: newDir,
    files,
    copies,
    github: githubReads(io, config, copies.branches, "build-cases"),
    gateway: readerGateway(
      config,
      given["key-file"],
      io.spendRecorder,
      budgetUsd === undefined ? undefined : Math.max(0, budgetUsd - priorSpend.usd),
      concurrency
    ),
    placer: placing,
    servedIds: new Set(config.servedModels.map(modelId)),
    readStore: oneAtATime,
  };
  const caseRecords: Record<string, unknown>[] = [];
  const sessions: SessionRecord[] = [];
  let casesBuilt = 0;
  // The sessions decided so far, the earlier runs' among them, for the stop rule.
  let decidedSessions = 0;
  let stopped: string | undefined;
  let ruleStop: RuleStop | undefined;
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
        githubRetries: run.github.retries.record(prior?.githubRetries),
        readerSpend: addSpend(priorSpend, run.gateway.spend()),
        listing: listingCounts(
          caseRecords.map((each) => ListingMeasure.safeParse(each.listing).data)
        ),
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
      const made = await caseOf(run, store, transcriptPath, spool, record, wanted);
      if (!("case" in made)) return made;
      // The listing measure: a top-level session's is against the earliest subagent of it spawned
      // under the build its profile is read at, which caseOf read from the store before it built
      // the profile, whose pools that listing decided.
      const measure = listingMeasure(made.listing, made.read);
      return {
        ...made,
        case: { ...made.case, evidence: withMeasure(made.case.evidence ?? [], measure) },
      };
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
  // A throw from the loop (a write that fails, an io that ends the run) closes and awaits every
  // open session first, so none goes on pulling into the spool after the build has given it up.
  try {
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
          listing: measureIn(built.evidence) ?? null,
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
  } catch (error) {
    await closeOpen("not-needed");
    throw error;
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
    contextSession = contextSessionOption(named, config);
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
      github: githubReads(io, config, copies.branches, "build-tasks"),
      placer: placing,
    },
  };
}

/** The session `named` (`--context-session`'s `<store>:<hash>`) names, its store one of
 *  `config`'s. Refuses another form, and a store the configuration does not name. */
function contextSessionOption(
  named: string,
  config: SkillCutsConfig
): { readonly store: string; readonly hash: string } {
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
  return { store, hash };
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
 *  rebuilt by `sessionProfile` with `options`. Throws, naming why, when the session gives none. */
async function pulledProfile(
  run: ProfileRun,
  store: string,
  filePath: string,
  spool: string,
  skill: string,
  options: SessionProfileOptions = {}
): Promise<SessionProfile> {
  const t = await pulledTranscript(run.config.psqlCommand, store, filePath, spool);
  return sessionProfile(run, storeNamed(run.config, store), filePath, t, skill, options);
}

/** The context session of a run and its profile, the one each written task takes; its evidence
 *  holds its listing measure. */
interface ContextProfile {
  readonly store: string;
  readonly hash: string;
  readonly built: SessionProfile;
}

/** The context session's profile, rebuilt before any reader call, or undefined when the plan
 *  names no context session. Throws when the store holds the session no longer or it gives no
 *  profile, and when its profile does not list the skill (`rebuiltContext`). */
async function contextProfile(
  plan: ProfilesPlan,
  spool: string,
  skill: string
): Promise<ContextProfile | undefined> {
  if (plan.contextSession === undefined) return undefined;
  const { store, hash } = plan.contextSession;
  const named = `--context-session ${store}:${hash}`;
  const built = await rebuiltContext(plan.run, plan.contextSession, named, spool, skill);
  if (built === undefined) throw new Error(`${named}: sessions.sql gives no file of this hash`);
  return built;
}

/** The profile of the context session `session`, pulled into `spool` and rebuilt, or undefined
 *  when sessions.sql gives no file of its hash (its store holds it no longer). A top-level context
 *  session is read at the build its listing measure's subagent was spawned under (TOP_BUILD_RULE):
 *  the earliest subagent whose recorded prompt holds a listing, found first (one subagents.sql
 *  query), so the measure compares the profile with a listing of the same build. Throws, naming it
 *  `named`, when the session gives no profile, and when its profile does not list the skill,
 *  since a written task in it would test nothing: both are found after a store read, so the
 *  command fails (exit 1), not refuses. */
async function rebuiltContext(
  run: ProfileRun,
  session: { readonly store: string; readonly hash: string },
  named: string,
  spool: string,
  skill: string
): Promise<ContextProfile | undefined> {
  const { store, hash } = session;
  const { paths } = await resolvePaths(run.config.psqlCommand, store, [hash]);
  const filePath = paths.get(hash);
  if (filePath === undefined) return undefined;
  const filePathHash = pathHash(filePath);
  const open = { from: null, to: null };
  let found: SubagentsFound | undefined;
  let unplaced: string | undefined;
  let built: SessionProfile;
  try {
    const listings = await subagentListings(run.config.psqlCommand, store, [
      { hash: filePathHash, ...open },
    ]);
    found = listings.get(betweenKey(filePathHash, open));
    const listing = found?.listing;
    const placed = listing !== undefined && !Number.isNaN(Date.parse(listing.start ?? ""));
    if (listing !== undefined && !placed) {
      unplaced = `its earliest subagent whose recorded prompt holds a skill listing (${listing.pathHash}) records no start, so the build it was spawned under is not known (subagents.sql)`;
    }
    built = await pulledProfile(run, store, filePath, spool, skill, {
      asContext: placed
        ? { spawned: { pathHash: listing.pathHash, at: listing.start ?? "" } }
        : {
            unspawned:
              unplaced ??
              (found === undefined
                ? "the store holds no subagent of it (subagents.sql, asked for those spawned at any time)"
                : `none of its ${found.subagents} subagents records a skill listing (subagents.sql, asked for those spawned at any time)`),
          },
      read: unplaced === undefined ? { found } : { unread: unplaced },
    });
  } catch (error) {
    throw new Error(`${named}: ${messageFor(error)}`);
  }
  if (!built.listed) {
    // Found after a store read and a pull, so it fails the run (exit 1): exit 2 is a refusal
    // before any store read.
    throw new Error(
      `${named}: its rebuilt context (${built.profile.context}) does not list ${skill}, so a written task in it would test nothing; give a session whose context lists it`
    );
  }
  built.evidence.push(listingMeasure(built.listing, built.read));
  return { store, hash, built: withRecordedListing(built, built.read) };
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
 * test nothing. Returns the tasks kept, the record of each task, the store queries it made
 * (sessions.sql for the paths) and those each session task's profile made (subagents.sql for its
 * listing, one a session, read as the profile was rebuilt), and the counts of the measures.
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
): Promise<{
  tasks: T[];
  notes: Record<string, unknown>[];
  queries: number;
  listing: Record<string, number>;
}> {
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
  // Each task's profile: the context session's for a written task; for a task from a session, one
  // rebuilt already, when the session was a candidate, else one rebuilt now; or why it has none.
  const profiles = new Map<string, { built?: SessionProfile; failed: string }>();
  for (const task of tasks) {
    let built: SessionProfile | undefined;
    let failed = "no --context-session gives a written task its profile";
    if (task.source === "written") built = context?.built;
    else {
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
    profiles.set(task.id, { built, failed });
  }
  // A session that records no prompt is measured against the earliest subagent of it spawned
  // under the build its profile is read at whose recorded prompt holds a listing
  // (`listingMeasure`), which its profile read from the store as it was rebuilt, since that
  // listing decides its pools: one subagents.sql query a session.
  for (const task of tasks) {
    const built = task.source === "session" ? profiles.get(task.id)?.built : undefined;
    if (built !== undefined && "found" in built.read) queries += 1;
  }
  const measures: (ListingMeasure | undefined)[] = [];
  const notes: Record<string, unknown>[] = [];
  const out: T[] = [];
  for (const task of tasks) {
    const { built, failed } = profiles.get(task.id) ?? { built: undefined, failed: "" };
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
    // A written task's profile is the context session's, which keeps its listing already.
    const listed = task.source === "written" ? built : withRecordedListing(built, built.read);
    const file = path.resolve(plan.dir, `${task.id}.json`);
    writeWhole(file, json(listed.profile));
    const evidence =
      task.source === "written"
        ? built.evidence
        : withMeasure(built.evidence, listingMeasure(built.listing, built.read));
    measures.push(measureIn(evidence));
    notes.push({
      task: task.id,
      from: task.source === "written" ? "context-session" : "session",
      profile: file,
      context: built.profile.context,
      skills: built.profile.skills.length,
      ...(task.source === "written" ? {} : { secretsRemoved: listed.secretsRemoved, evidence }),
    });
    out.push({ ...task, profile: file });
  }
  return { tasks: out, notes, queries, listing: listingCounts(measures) };
}

/** What a summary records of a run's profiles: where they are, the context session (with the
 *  evidence of its profile), each task's, the store queries `writeProfiles` made (sessions.sql for
 *  the paths, subagents.sql for the listing measures), the measures' counts, and the GitHub reads
 *  sent again after a transient failure, by reason. */
function profilesRecord(
  plan: ProfilesPlan,
  context: ContextProfile | undefined,
  written: { notes: Record<string, unknown>[]; queries: number; listing: Record<string, number> }
): Record<string, unknown> {
  return {
    dir: path.resolve(plan.dir),
    contextSession:
      context === undefined
        ? null
        : {
            store: context.store,
            hash: context.hash,
            pathHash: context.built.pathHash,
            context: context.built.profile.context,
            secretsRemoved: context.built.secretsRemoved,
            evidence: context.built.evidence,
          },
    tasks: written.notes,
    sessionQueries: written.queries,
    listing: written.listing,
    githubRetries: plan.run.github.retries.record(),
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
        // The pull, the parse and the profile each drop the candidate with the reason
        // `--first-read` gives the same step.
        let pulledBytes: number;
        try {
          pulledBytes = (await pullTranscript(config.psqlCommand, row.store, filePath, spool))
            .bytes;
        } catch (error) {
          drop("pull-failed", messageFor(error));
          continue;
        }
        if (pulledBytes === 0) {
          drop("pull-failed", "the pull returned no row");
          continue;
        }
        let t: Transcript;
        try {
          t = readTranscript(spool);
        } catch (error) {
          drop("unparseable", messageFor(error));
          continue;
        }
        const ran = model === undefined ? undefined : firstReplyModel(t);
        let profile: SessionProfile | undefined;
        if (plan !== undefined && ran === model) {
          try {
            profile = await sessionProfile(
              plan.run,
              storeNamed(config, row.store),
              filePath,
              t,
              skill
            );
          } catch (error) {
            drop("profile-failed", messageFor(error));
            continue;
          }
        }
        session = { model: ran, profile };
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

// ---------------------------------------------------------------------------------------------
// build-tasks --reprofile

/** The command a `--reprofile` summary names. */
const REPROFILE_COMMAND = "build-tasks --reprofile";
/** The item of a reprofiled task's evidence when its store holds its session no longer. */
const NO_SESSION = "no session in the store";
/** The flags of the candidate search and the tasks reader, none of which `--reprofile` takes. */
const REPROFILE_REFUSES = [
  "key-file",
  "skill",
  "store",
  "out",
  "first-read",
  "sessions",
  "seed",
  "max",
  "model",
  "max-candidates",
  "max-search-minutes",
] as const;

/** What `--reprofile` reads of a tasks file's summary (`<tasks>.summary.json`, as build-tasks or
 *  `--reprofile` wrote it): the tested skill, and, when the tasks have profiles, the context
 *  session and the evidence of each task's profile. */
const ProfiledSummary = z.looseObject({
  command: z.enum(["build-tasks", REPROFILE_COMMAND]),
  skill: z.string().min(1),
  profiles: z
    .looseObject({
      contextSession: z
        .looseObject({
          store: z.string(),
          hash: z.string(),
          evidence: z.array(EvidenceItem).optional(),
        })
        .nullable(),
      tasks: z.array(
        z.looseObject({ task: z.string(), evidence: z.array(EvidenceItem).optional() })
      ),
    })
    .optional(),
});

/** The tasks of `file`, one JSON line each (load.ts's Task). Refuses a line that is no task, an id
 *  given twice (an id names its profile's file) and a file of no task. */
function tasksToReprofile(file: string): Task[] {
  const tasks: Task[] = [];
  const faults: string[] = [];
  for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
    if (line.trim() === "") continue;
    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch (error) {
      faults.push(`line ${index + 1} is not JSON: ${messageFor(error)}`);
      continue;
    }
    const parsed = Task.safeParse(raw);
    if (!parsed.success) {
      faults.push(`line ${index + 1} is no task: ${schemaFaults(parsed.error)}`);
    } else if (tasks.some((task) => task.id === parsed.data.id)) {
      faults.push(`line ${index + 1}: the task id ${parsed.data.id} is given twice`);
    } else {
      tasks.push(parsed.data);
    }
  }
  if (faults.length > 0) throw new Refused(`--reprofile ${file}: ${faults.join("; ")}`);
  if (tasks.length === 0) throw new Refused(`--reprofile ${file} holds no task`);
  return tasks;
}

/**
 * `build-tasks --reprofile`: the profile of each task of a tasks file built again as
 * `--profiles-out` builds one now, with no candidate search and no reader call, so the tasks a
 * bounded search chose stay as it chose them. A session task's session is found in its store by
 * its path hash (session-paths.sql, one query a store) and pulled as the search pulls it
 * (`pulledProfile`), the prompt builds of the roots of those whose old profile records a prompt (a
 * subagent's) read in one digest-times.sql query a store; a written task takes the profile of
 * `--context-session`, else of the context session the tasks file's summary names
 * (`rebuiltContext`). A task whose session its store holds no longer keeps its old profile, and
 * its evidence says so (NO_SESSION). A session task whose rebuilt context no longer lists the
 * skill is left out and named, and the command fails (exit 1); a store read, a pull or a rebuild
 * that fails fails it before anything is written. Writes each profile to `<dir>/<task id>.json`,
 * the tasks to `<dir>/tasks.jsonl`, each `profile` naming its new file, and
 * `<dir>/tasks.jsonl.summary.json`: per task, whether the profile's bytes changed and the
 * evidence items added and removed against the old summary's.
 */
async function reprofileTasks(
  values: Readonly<Record<string, string | boolean | string[] | undefined>>,
  io: BuildIo
): Promise<number> {
  const given = required(
    values,
    ["config", "copies", "count", "reprofile", "profiles-out"],
    BUILD_TASKS_USAGE
  );
  for (const flag of REPROFILE_REFUSES) {
    if (values[flag] !== undefined) {
      throw new Refused(
        `--reprofile builds the profiles of a tasks file again with no candidate search and no reader call, so it takes no --${flag}`
      );
    }
  }
  const dir = path.resolve(given["profiles-out"]);
  const there = statSync(dir, { throwIfNoEntry: false });
  if (there !== undefined && !there.isDirectory()) {
    throw new Refused(`--profiles-out ${dir} is a file; give a new directory`);
  }
  if (there !== undefined && readdirSync(dir).length > 0) {
    throw new Refused(`--profiles-out ${dir} holds files already; give a new directory`);
  }
  const config = configFrom(given.config);
  const from = given.reprofile;
  mustBeFile(from, "--reprofile");
  const summaryFile = `${from}.summary.json`;
  mustBeFile(summaryFile, "--reprofile's summary");
  const parsed = ProfiledSummary.safeParse(readJson(summaryFile));
  if (!parsed.success) {
    throw new Refused(`${summaryFile} is no build-tasks summary: ${schemaFaults(parsed.error)}`);
  }
  const { skill, profiles: before } = parsed.data;
  const tasks = tasksToReprofile(from);
  // Each task's old profile, read before any store read, and the evidence the summary gives it.
  const noted = new Map((before?.tasks ?? []).map((note) => [note.task, note.evidence ?? []]));
  const recorded = before?.contextSession ?? undefined;
  const contextBefore: Evidence[] = recorded?.evidence ?? [];
  const old = new Map<string, { readonly file: string; readonly text: string }>();
  for (const task of tasks) {
    if (task.source === "session") {
      try {
        storeNamed(config, task.store);
      } catch (error) {
        throw new Refused(`task ${task.id}: ${messageFor(error)}`);
      }
    }
    if (task.profile === undefined) continue;
    const file = path.resolve(path.dirname(from), task.profile);
    mustBeFile(file, `task ${task.id}'s profile`);
    old.set(task.id, { file, text: readFileSync(file, "utf8") });
  }
  // The session whose profile each written task takes.
  const written = tasks.some((task) => task.source === "written");
  const named = values["context-session"];
  let contextSession: { readonly store: string; readonly hash: string } | undefined;
  if (typeof named === "string") {
    if (!written) {
      throw new Refused(`${from} holds no written task, so --reprofile takes no --context-session`);
    }
    contextSession = contextSessionOption(named, config);
  } else if (written) {
    if (recorded === undefined) {
      throw new Refused(
        `${from} holds written tasks, and ${summaryFile} names no context session whose profile they take; give --context-session <store>:<hash>`
      );
    }
    contextSession = contextSessionOption(`${recorded.store}:${recorded.hash}`, config);
  }
  const { copies, placer: placing } = readCopiesAndPlacer(config, given.count, given.copies);
  const run: ProfileRun = {
    config,
    copies,
    github: githubReads(io, config, copies.branches, "build-tasks"),
    placer: placing,
  };

  const spools = spoolDir(dir, io);
  const spool = path.join(spools, "profile.jsonl");
  let sessionQueries = 0;
  let context: ContextProfile | undefined;
  let contextGone: Evidence | undefined;
  if (contextSession !== undefined) {
    const { store, hash } = contextSession;
    const label = `the context session ${store}:${hash}`;
    context = await rebuiltContext(run, contextSession, label, spool, skill);
    // Its path (sessions.sql), and its subagent whose build its profile is read at (subagents.sql).
    sessionQueries += context === undefined ? 1 : 2;
    if (context === undefined) {
      contextGone = {
        item: NO_SESSION,
        source: `the store ${store} holds no file of the hash ${hash} now (sessions.sql), so each written task keeps its old profile`,
      };
    }
  }
  // Each session task's path, by its path hash: one session-paths.sql query a store.
  const paths = new Map<string, string>();
  const sessionTasks = tasks.flatMap((task) => (task.source === "session" ? [task] : []));
  for (const store of new Set(sessionTasks.map((task) => task.store))) {
    const hashes = sessionTasks.filter((task) => task.store === store).map((task) => task.pathHash);
    const unique = [...new Set(hashes)];
    const resolved = await resolvePaths(config.psqlCommand, store, unique, sessionPathsSql);
    sessionQueries += resolved.queries;
    for (const [hash, filePath] of resolved.paths) paths.set(`${store}\0${hash}`, filePath);
  }
  // A subagent's profile is read at its root's prompt builds (PARENT_LOAD_RULE): the
  // digest-times.sql rows of the session tasks whose old profile records a system prompt (a
  // subagent's) are read in one query a store, sent as the first of them is rebuilt; any other
  // session that needs its row is asked alone (`sessionProfile`).
  const promptHashes = new Map<string, string[]>();
  for (const task of sessionTasks) {
    const profile = parseJson(old.get(task.id)?.text ?? "");
    if (!paths.has(`${task.store}\0${task.pathHash}`)) continue;
    if (!isObject(profile) || profile.systemPrompt === undefined) continue;
    promptHashes.set(task.store, [...(promptHashes.get(task.store) ?? []), task.pathHash]);
  }
  const digestBatches = new Map<string, Promise<Map<string, DigestRow>>>();
  const digestRowOf = (store: string) => async (hash: string) => {
    const batch = promptHashes.get(store) ?? [];
    if (!batch.includes(hash)) {
      return (await digestRows(config.psqlCommand, store, [hash])).get(hash);
    }
    let rows = digestBatches.get(store);
    if (rows === undefined) {
      rows = digestRows(config.psqlCommand, store, batch);
      digestBatches.set(store, rows);
    }
    return (await rows).get(hash);
  };
  // Each session task's profile built again (`pulledProfile`), or why the task is left out: a
  // profile load would refuse is not written, nor one whose context no longer lists the skill. A
  // session that records no prompt is measured against the earliest subagent of it spawned under
  // the build its profile is read at whose recorded prompt holds a listing (`listingMeasure`),
  // which its profile reads from the store as it is rebuilt, since that listing decides its
  // pools: one subagents.sql query a session.
  const pulled = new Map<string, SessionProfile | { readonly leftOut: string }>();
  for (const task of sessionTasks) {
    const filePath = paths.get(`${task.store}\0${task.pathHash}`);
    if (filePath === undefined) continue;
    let built: SessionProfile;
    try {
      built = await pulledProfile(run, task.store, filePath, spool, skill, {
        digestRow: digestRowOf(task.store),
      });
    } catch (error) {
      if (error instanceof ProfileRefused || error instanceof PodNotMeasurable) {
        pulled.set(task.id, { leftOut: error.message });
        continue;
      }
      throw new Error(
        `task ${task.id} (${task.store} ${task.pathHash.slice(0, 12)}): ${messageFor(error)}`
      );
    }
    if ("found" in built.read) sessionQueries += 1;
    pulled.set(
      task.id,
      built.listed
        ? built
        : {
            leftOut: `its rebuilt context ${built.profile.context} does not list ${skill}, so it would test nothing`,
          }
    );
  }
  /** The measure of a task that keeps its old profile `text`: not measurable, for `why`, listing
   *  the profile's skills (KeptSkills), each under the name Oh My Pi lists it by (`listedName`). */
  const keptMeasure = (text: string | undefined, why: string) => {
    const kept = KeptSkills.safeParse(parseJson(text ?? ""));
    return listingMeasure(
      {
        listed: kept.success ? kept.data.skills.map((each) => listedName(each)) : [],
        recorded: undefined,
        sessionDigest: null,
      },
      { unread: why }
    );
  };
  const measures: (ListingMeasure | undefined)[] = [];

  const same = (a: Evidence, b: Evidence) => a.item === b.item && a.source === b.source;
  const lines: Task[] = [];
  const files = new Map<string, string>();
  const records: Record<string, unknown>[] = [];
  const leftOut: { task: string; why: string }[] = [];
  const counts = { changed: 0, unchanged: 0, kept: 0, leftOut: 0 };
  for (const task of tasks) {
    const was = old.get(task.id);
    const evidenceBefore = task.source === "written" ? contextBefore : (noted.get(task.id) ?? []);
    // The profile built again and its evidence, or why the store gives none (`gone`).
    let rebuilt: { readonly text: string; readonly evidence: Evidence[] } | undefined;
    let gone: Evidence | undefined;
    if (task.source === "written") {
      gone = contextGone;
      if (context !== undefined) {
        rebuilt = { text: json(context.built.profile), evidence: context.built.evidence };
      }
    } else {
      const filePath = paths.get(`${task.store}\0${task.pathHash}`);
      if (filePath === undefined) {
        gone = {
          item: NO_SESSION,
          source: `the store ${task.store} holds no file of the path hash ${task.pathHash} now (session-paths.sql), so the task keeps its old profile`,
        };
      } else {
        const built = pulled.get(task.id);
        if (built === undefined || "leftOut" in built) {
          const why = built?.leftOut ?? "its profile was not rebuilt";
          leftOut.push({ task: task.id, why });
          io.err(`build-tasks: ${task.id}: left out: ${why}`);
          continue;
        }
        rebuilt = {
          text: json(withRecordedListing(built, built.read).profile),
          evidence: withMeasure(built.evidence, listingMeasure(built.listing, built.read)),
        };
      }
    }
    const text = rebuilt?.text ?? was?.text;
    const evidence = rebuilt?.evidence ?? [
      ...evidenceBefore.filter((each) => each.item !== LISTING_MEASURE_ITEM),
      ...(gone === undefined ? [] : [gone]),
      keptMeasure(was?.text, gone?.source ?? "the task keeps its old profile"),
    ];
    measures.push(measureIn(evidence));
    const outcome = rebuilt === undefined ? "kept" : text === was?.text ? "unchanged" : "changed";
    counts[outcome] += 1;
    const file = path.join(dir, `${task.id}.json`);
    if (text !== undefined) files.set(file, text);
    lines.push({ ...task, profile: text === undefined ? undefined : file });
    const added = evidence.filter((each) => !evidenceBefore.some((other) => same(each, other)));
    const removed = evidenceBefore.filter((each) => !evidence.some((other) => same(each, other)));
    records.push({
      task: task.id,
      from: task.source === "written" ? "context-session" : "session",
      was: was?.file ?? null,
      profile: text === undefined ? null : file,
      outcome,
      ...(task.source === "session" ? { evidence } : {}),
      differs: { added, removed },
    });
    io.out(
      `build-tasks: ${task.id}: profile ${outcome}${
        outcome === "changed"
          ? `: ${added.length} evidence items added, ${removed.length} removed`
          : gone === undefined
            ? ""
            : `: ${gone.source}`
      }\n`
    );
  }
  rmSync(spools, { recursive: true, force: true });
  for (const [file, text] of files) writeWhole(file, text);
  const tasksFile = path.join(dir, "tasks.jsonl");
  writeWhole(tasksFile, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
  counts.leftOut = leftOut.length;
  const summaryOut = `${tasksFile}.summary.json`;
  writeWhole(
    summaryOut,
    json({
      command: REPROFILE_COMMAND,
      from: path.resolve(from),
      skill,
      counts,
      listing: listingCounts(measures),
      leftOut,
      profiles: {
        dir,
        contextSession:
          contextSession === undefined
            ? null
            : {
                ...contextSession,
                pathHash: context?.built.pathHash ?? null,
                context: context?.built.profile.context ?? null,
                secretsRemoved: context?.built.secretsRemoved ?? null,
                evidence: context?.built.evidence ?? [
                  ...contextBefore,
                  ...(contextGone === undefined ? [] : [contextGone]),
                ],
              },
        tasks: records,
        sessionQueries,
        githubRetries: run.github.retries.record(),
      },
    })
  );
  io.out(
    `build-tasks: ${lines.length} of ${tasks.length} tasks written with their profiles built again (${counts.changed} changed, ${counts.unchanged} unchanged, ${counts.kept} kept because their store holds their session no longer) to ${tasksFile}, the profiles beside it, and ${summaryOut}\n`
  );
  if (leftOut.length > 0) {
    io.err(
      `build-tasks: failed: ${leftOut.length} tasks left out: ${leftOut.map((each) => each.task).join(", ")}`
    );
    return EXIT.failed;
  }
  return EXIT.ok;
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
      reprofile: { type: "string" },
    },
    BUILD_TASKS_USAGE
  );
  if (values.reprofile !== undefined) return reprofileTasks(values, io);
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
  const spools = plan === undefined && model === undefined ? undefined : spoolDir(given.out, io);
  const spool = spools === undefined ? "" : path.join(spools, "profile.jsonl");
  const context = plan === undefined ? undefined : await contextProfile(plan, spool, skill.name);
  const gateway = readerGateway(config, given["key-file"], io.spendRecorder);
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
 * (`messageLoad`: an Oh My Pi `read` of `skill://<name>` or its SKILL.md; the stores hold
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
  const spools = spoolDir(given.out, io);
  const spool = path.join(spools, "session.jsonl");
  const gateway = readerGateway(config, given["key-file"], io.spendRecorder);
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
    // A pull that returns no row is dropped as build-cases and offeredMessages drop it.
    if (pulled.bytes === 0) return no("pull-failed", "the pull returned no row");
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
    const load = messageLoad("omp", blocks, { name: skill.name });
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
 *  name. Returns the exit status (EXIT). The usage spans its reader calls queued are its caller's
 *  to wait for (`io.spendRecorder`'s `flush`), so that a process waits once: cli.ts's `main` does
 *  for every command, and this file's own run once `main` has returned. */
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
  } finally {
    releaseSpools(io);
  }
}

if (import.meta.main) {
  const spendRecorder = processSpendRecorder();
  const code = await main(Bun.argv.slice(2), {
    out: (text) => process.stdout.write(text),
    err: (line) => process.stderr.write(`${line}\n`),
    gh: spawnGh,
    now: () => performance.now(),
    spendRecorder,
  });
  await spendRecorder.flush();
  process.exit(code);
}
