/**
 * The load test (README.md, "load"): whether a host's agent loads a skill for a task,
 * given what the host lists for the skill. `skill-cuts load` (cli.ts) runs it:
 *
 *     bun scripts/skill-cuts/cli.ts load --config <file> --key-file <file> --skill <dir>
 *         [--name <name>] --tasks <file> --host omp|claude --profile <file> [--runs 3] --out <dir>
 *         [--hint-draw <n>] [--test-setup <owner/repo>@<commit>:<path>]
 *
 * - `--skill` is the skill's directory as written: its description is the one tested. The skill
 *   is tested under `--name` when one is given, else under its frontmatter `name` (its
 *   directory's name when the frontmatter names none). `--name` changes the frontmatter `name`
 *   and the directory's name in a scratch copy (`renamedSkill`), and, for a profile that carries
 *   a subagent's recorded prompt, the name of that prompt's listing line of the skill
 *   (capture.ts's `renamedListingBlocks`), and nothing else: the skill's own text, its
 *   references, every other line of a recorded prompt and every other file that names the skill
 *   stay as they are, and the command says so.
 * - The tasks file (`loadTasks`) holds one JSON object per line (Task): `{id, text, label,
 *   source: "session", store, pathHash}` for a real session's first request (its store, and
 *   cases.ts's `pathHash` of the transcript's path, never the path), or `{id, text, label,
 *   source: "written"}`; `label` is `need` or `near-miss`. Either may name its `profile`: the
 *   profile file of the session the task came from (absolute, or relative to the tasks file's
 *   directory), so that the task runs in that session's rebuilt context.
 * - A profile file (`loadProfile`) is one JSON object: cases.ts's `Profile` (the other skills
 *   installed beside it, each a `ProfileSkill`; the Oh My Pi plugins; the context files; an
 *   appended system prompt), the `context` (a case's: `devbox`, the default,
 *   `legion:<project>:<role>` or `subagent:<agent>`), for Claude Code `claude`, the plugin
 *   layout (ClaudeLayout), and `recordedListing`, the skill listing its session recorded, where
 *   build.ts kept one (RecordedListing). `--profile` is the profile of each task that names
 *   none; a task that names none when no `--profile` is given is refused. A profile skill with a
 *   `namespace` (a copy of another source that Oh My Pi lists as `<namespace>/<name>`) is that
 *   name in every check, so it may share the tested skill's name; a Claude Code profile that
 *   holds one is refused, since its layout places skills by their bare name.
 * - `--test-setup` reads the tested copy from its inventory commit and installs only that
 *   additional copy. A labelled task may already stage it. Otherwise a profile that installs it
 *   is refused, as is an unreadable copy. The flag is required for labelled tasks.
 *   The run records, summary and verdict say "test setup, not observed use". Oh My Pi draws
 *   the tested name's hint on each draw instead of giving it a recorded line.
 *
 * For each task the host's first request is captured once, in the rebuilt context of the task's
 * own profile (its skills, its plugins with capture.ts's stand-ins, its context files, its
 * appended prompt and its context), against the recorder (recorder.ts), so no host process
 * reaches the gateway or runs a tool. A Legion task's role prompt is composed as a behaviour
 * case's is, at the commit its daemon image was built from where the task's evidence in the tasks
 * file's build-tasks summary names one (the item `the daemon image's commit of <repo>`, build.ts),
 * else at the profile's commit of its plugin repository; that item is the only one of the
 * evidence the capture is given. A saved capture whose record says its role prompt was composed
 * at another commit than the task's evidence names now (`rolePromptFault`; a record with no
 * `commitOf` was composed at the plugin's commit) is refused before any run, as one made under a
 * profile of other bytes is:
 *
 * - Oh My Pi: capture.ts's `captureBody`, with the task's text as a history of one user message.
 *   Oh My Pi lists each skill under a hint that it has a model write from the name and the
 *   description, so what is tested is that hint; the capture writes it once per Oh My Pi version,
 *   name and description, through the gateway, and keeps it in its cache. The hint is made from
 *   the name too, so the two arms of a name test get different hints by design: that is part of
 *   the name's effect. Where the profile keeps the listing its session recorded, each skill is
 *   listed under its recorded line in place of a hint (the architect, 2026-11-23T07:11Z;
 *   `recordedLines`): each other skill's, and the tested skill's only where the arm lists it
 *   under the name and the description the session's copy had (read from its repository at its
 *   commit), so a proposed name, or a text that differs from the session's, gets a drawn hint.
 *   `--hint-draw <n>` (1 by default) draws each other hint again: draw n > 1 is cached apart from
 *   draw 1 (capture.ts, "The draw"), so a verdict that leads to action can rest on two draws. A
 *   `subagent:<agent>` profile is captured as capture.ts captures a subagent's case: its
 *   recorded prompt with only the skill's listing line changed, its recorded tools (each one
 *   nothing defines left out and recorded), and the stand-in Envoy listener.
 * - Claude Code: for each profile, a scratch `CLAUDE_CONFIG_DIR` and HOME, into which `claude
 *   plugin install` installs a local marketplace that holds the profile's skills and the tested
 *   skill under its name, each in its plugin; then `claude -p --output-format stream-json
 *   --verbose --model <id> -- <task>` in the context's working directory (context.ts: the
 *   profile's `workingDirectory` in its checkout of the session repository), with
 *   `ANTHROPIC_BASE_URL` at the recorder. A profile's user-level context file (`user: true`, the
 *   one its dev box's host read; build.ts) goes in as Claude Code's user memory, `CLAUDE.md` in
 *   the scratch `CLAUDE_CONFIG_DIR`, from its repository at its commit, and not into the checkout;
 *   the record names it (`userMemory`). Its other context files (the session repository's, at
 *   their paths) go into the checkout, as context.ts writes them, where Claude Code reads a
 *   project's `CLAUDE.md` of the working directory and its parents. A project skill of the
 *   profile's `sessionRepository` (`claudeKind`: that repository at that commit, at
 *   `<level>/.claude/skills/<name>`, `<level>` the working directory or a parent of it in the
 *   checkout) goes in no plugin: its directory is written at that path in the checkout, where
 *   Claude Code reads a project's skills, and lists it under its bare name. One at
 *   `<level>/.omp/skills/<name>` is left out, since Claude Code reads no `.omp/skills`. A profile
 *   that names no session repository has no project skill. The record names each skill's place
 *   (`placements`). The first request that holds tools and the task's text is kept. Claude Code
 *   lists each plugin skill as `<plugin>:<name>` with its description (`claudeListing`). Thinking
 *   `off` sets `CLAUDE_CODE_DISABLE_THINKING`, a level of `claude --effort` goes as that flag,
 *   and `auto` leaves Claude Code's own default.
 *
 * A skill that reaches a session other than by its listing line and a read (arms.ts's
 * `Injection`) is in that session's context whatever its listing says, so for it a load test
 * measures whether the agent reads a skill whose text it already holds. Each name it is tested
 * under gets the same block:
 *
 * - The Envoy plugin's dispatch-first (arms.ts's PLUGIN_INJECTED), in a profile that installs the
 *   plugin, on Oh My Pi: each capture's messages carry the plugin's block before the task's text,
 *   built from the tested directory as the plugin builds it (`withPluginBlock`; its frontmatter,
 *   which holds the name, is not in it), and under another name a hidden stand-in of the file the
 *   plugin reads keeps it loading (context.ts). A profile that also installs a copy under the
 *   skill's own name while it is tested under another is refused: the plugin would put that copy
 *   into the requests, not the tested text. On Claude Code it is refused: Claude Code's Envoy
 *   plugin puts it into a session with Dispatch from its session-start hook
 *   (packages/claude-envoy/hooks/session-hook.ts), which a load test's Claude Code does not run,
 *   and a profile does not say whether its session had that plugin.
 * - A skill a subagent's agent definition autoloads (`autoloadSkills`, read as Oh My Pi reads it:
 *   the dev box's user agents its installers link, then the profile's plugins, each at its commit;
 *   context.ts's `subagentDefinition`): a `subagent:<agent>` profile
 *   whose agent autoloads the skill is refused on Oh My Pi, before any model call. The subagent
 *   gets the skill's text before its task whatever the listing says, and under another name its
 *   agent would autoload none.
 *
 * In any other profile such a skill is listed like any other and tested as one.
 *
 * Then that one body is sent to the gateway (gateway.ts) through replay.ts's `replayOnce`, with the
 * `anthropic-*` headers the host sent beside it, on the route of its API: an `anthropic/<id>`
 * model's body goes to the Messages route, and an `openai/<id>` model's (Oh My Pi only; its hints
 * still come from an Anthropic `--hint-model`) to the Responses route. A reply loads the skill when
 * one of its tool calls reads it (`messageLoad`, over the reply's tool calls in either API:
 * `tool_use` blocks, or `function_call` items with their arguments parsed): for Oh My Pi, a `read`
 * of `skill://<name>`, `skill://<name>/SKILL.md` or a path ending `/<name>/SKILL.md`; for Claude
 * Code, a `Skill` call naming `<name>` or `<plugin>:<name>`, or a `Read` of a path ending
 * `/<name>/SKILL.md`. These are the reads count.sql counts: a read selector after the last `/`
 * (`:raw`, `:1-40`) is taken off first, and a read of a reference is no load. One tool call of
 * the reply that reads the skill is enough, whatever comes before it.
 *
 * Skill reads follow the behaviour replay's rule (the architect, 2026-11-22T18:58Z and
 * 18:59:51Z). On Oh My Pi, a reply whose every tool call reads a skill file is answered as
 * replay.ts answers one: a read of another skill the profile installs from that skill's installed
 * copy (context.ts's `installedCopies` of the capture's record, made again from its repository at
 * its commit by `restoredCapture` where the cache lacks it, which first refuses a capture whose
 * record names other copies than the task's profile now gives, cached or not), a read of a skill
 * it does not install with Oh My Pi's unknown-skill result, and a read of another file of the
 * tested skill from its directory. Then the body goes again with the reply and the answers, at
 * most MAX_SKILL_ROUNDS times. A reply that reads skill files beside other calls, and does not
 * read the tested skill, is sorted with no model by replay.ts's LOAD_RULE (the runner's architect,
 * 2026-11-26T06:20:08Z): each other call is preparation when it only reads or looks (a `read`,
 * `grep`, `glob`, `find` or LSP lookup, a `write` to the `xd://lsp` device, a `bash` call each
 * of whose commands starts with a program of replay.ts's read-only list), an action when it
 * changes state or starts the task's work (a `write` or `edit` of a file, a `task` dispatch, any
 * other tool), and undecided otherwise (a `bash` command off the list). A reply with an action
 * ends the run there, as before; one with no action and an undecided call gets no verdict; one
 * whose other calls only prepare is a round of its own: its reads are answered and it goes back
 * holding only those calls, the others recorded as left out (LoadRun's `leftOut`), never run.
 * A run comes to one of five outcomes (RunOutcome):
 *
 * - `loaded`: a reply reads the skill, the first one or one after answered rounds, before any
 *   reply does more than read skill files and prepare. The run ends at that reply.
 * - `sent by`: the answer to a read of another skill holds a text that names the skill
 *   (`namesSkill`). What the agent does next comes from that text, not from the skill's listing,
 *   so the run ends once that answer is made, before it is sent, and names that skill (`sentBy`).
 * - `reads past last round`: the reply after the last answered round still only reads skill
 *   files, or reads them beside calls that only prepare (replay.ts's ReadsPastLastRound).
 * - `undecided`: a reply reads skill files beside a call the rule sorts as neither (replay.ts's
 *   UndecidedReply); the run's `reason` names each such call.
 * - `not loaded`: a run that ends at a reply with no read the replay answers, or with an action
 *   beside one, or that makes no tool call, with no load and no answer that names the skill
 *   before it.
 *
 * Claude Code's runs end at their first message (`skillReads: "first message"` in their record):
 * the runner does not rebuild Claude Code's result of a `Skill` or `Read` call, so it answers no
 * read there, and the first message is `loaded` or `not loaded`.
 *
 * `loaded` and `not loaded` runs are judged; for each run of the other three the task runs again,
 * up to MAX_EXTRA_RUNS (score.ts's) runs beyond `--runs`, until it has `--runs` judged runs
 * (`taskOutcome`). A task still short of them after its extra runs gives no description evidence:
 * the skill's verdict leaves it out, and its line and the summary say why, with how many of its
 * runs each skill sent, how many read past the last round and how many were undecided. Each
 * run's record holds each reply (ReplyRecord): its tool calls with their names and targets (a
 * read's path, a `Skill` call's skill) and, on Oh My Pi, what LOAD_RULE reads of a `bash` or
 * `write` call (its command, its path), the answers the runner made to its reads with where each
 * came from (replay.ts's AnsweredRead: the tested skill's directory, an installed copy, or no
 * copy), and what it used. Every reply but the last went back with its answers.
 * `load audit-mixed` (`auditLoadMixed`) applies the rule to the saved runs of an `--out`, with no
 * model call.
 *
 * A capture shows its task's session, not the run that captured it (capture.ts, "The harness
 * strings"): a task from a session stands for that session, and a written task for the session
 * `--context-session` names, whose context its profile was rebuilt from. That session's head is
 * read from its store once (`<out>/sessions/<store>-<pathHash>.json`); the capture shows its
 * working directory and the reminder Oh My Pi journaled for its first user turn (else one rendered
 * for that turn's UTC date), and Claude Code's body its working directory, its config directory's
 * memory path, that date and a neutral OS line (`claudeHarnessRules`). A written task with no
 * `--context-session` names no session: its values are neutral, its date the capture's own.
 *
 * The runs of a profile's tasks are sent together, once each task of it has its capture, in one
 * seeded order (`sendRuns`): no task's runs go as a batch of their own; the extra runs its tasks
 * then need go in a seeded order of their own, and so on until none is needed. Each run goes with
 * a session id of its own (gateway.ts's `freshIdentity`), which each of its requests keeps, since
 * a captured body holds the one id of the host run that made it, and a gateway that routes or
 * caches by that id makes it a factor of every run that sends it. A run of a Messages body also
 * goes with a routing id, on each of its requests in gateway.ts's ROUTING_HEADER, by which the
 * gateway pins a call's deployment (with none, every run of one body went to one deployment):
 * the id is derived, not drawn (`loadRoutingId`: the SHA-256 of `<seed>:<task id>:<run number>`
 * in a UUID's form, the seed `--route-seed`, empty by default), so the arms of a name or
 * description test, two load commands over the same tasks, send the same id for run n of a task,
 * and that run of each arm goes to one deployment, with no id passed between them. A Responses
 * body goes with none. Its body is otherwise its capture's, but for a cache breakpoint on its
 * last message (replay.ts's `withCacheBreakpoint`), so that a task's later runs read the whole
 * body from the cache.
 *
 * Under `--out`: `summary.json` (the test's settings, what the host lists for the skill, each
 * task's profile file with the SHA-256 of its bytes, its counts (TaskOutcome: judged runs, loads,
 * runs sent by each skill, runs that read past the last round, extra runs used) and verdict, why
 * it gives no description evidence (null when it gives some), why it was dropped (null when it
 * was not), its spend (TaskSpend), the line its capture lists the tested skill under and where
 * that line came from (`listedUnder`: capture.ts's HintSource, `recorded (session)`, `recorded
 * (subagent <path hash>)` or `drawn (draw <n>)`, or on Claude Code `description (Claude Code)`),
 * how many tasks list it under each, the skill's verdict, the test's spend, the route seed, and
 * the runs counted by the deployment that answered them, under the tested name
 * (`deployments.byName`; gateway.ts's `deploymentCounts`), which the command also prints),
 * `runs/<task id>.jsonl` (one JSON line per run, LoadRun, which names its task's profile file and
 * that SHA-256 too, its session id, its routing id, the deployment that answered each of its
 * requests and its place in its send order), `runs/<task id>.failed.jsonl` (one JSON line per
 * run that failed after one or more of its requests had their replies, FailedRun: the error and
 * what those replies used), `captures/` (each task's body, the body as the host sent it, and its
 * record) and `sessions/`. A failed run comes to no outcome and is made again by the next command;
 * what its replies used counts in its task's spend, its task's line and the test's spend, in this
 * command and every later one.
 * The refusals above for a skill a plugin or an agent definition puts into the session are made
 * before any model call, each naming the profile.
 * A task whose run file holds its judged runs, or has used its extra runs, is skipped, so a
 * command started again goes on from what was saved. A task whose run file holds runs but lacks
 * some makes them with the saved capture those runs were made with, never with one made again:
 * where that capture's body or record is gone, or it lists the tested skill under another line
 * than its runs record (LoadRun's `listed`, the one value a run keeps of its capture: a run records
 * no digest of the body), the command is refused before any capture or run (exit 2), naming the
 * task, its run file, the capture and both lines (`heldCaptureFault`). A task whose context
 * needs a directory no read can give (context.ts's UnreadableDirectory: the commit holds no such
 * directory, or GitHub
 * cuts a level of its tree short even read alone) is dropped: its line says so, `summary.json`
 * gives its reason as the task's `dropped` (`unreadable-directory: ` and the error, which names
 * the repository, the commit and the path), a command started again skips it while its profile
 * file keeps the same bytes, and the verdict rule sees it as a task with no runs. A capture that
 * fails for another reason, a read after its retries among them, is a failure (exit 1), tried
 * again by the next command.
 * An `--out` written by a test of other settings (another skill text, name, host,
 * model, thinking level, run count, hint model, hint draw, or one whose runs were sent before each
 * had its own session id, before the skill reads rule, or, on Oh My Pi, before recorded lines),
 * or holding a capture or runs of a task of the same id with another text, under a profile of
 * other bytes, or that its summary does not list (so the text they were made for is not known),
 * is refused. So is an `openai/<id>` model on Oh My Pi with no `--hint-model` when no task's
 * capture is saved: the hint model would be that model, and hints are written on the Messages
 * route only; and a `--hint-draw` above 1 on Claude Code, which lists descriptions and draws no
 * hint. The verdicts are score.ts's: a task's is `taskFault` and the skill's `descriptionFault`,
 * given only once every task has its judged runs or has used its extra runs, and those are
 * LOAD_RUNS. Each task's line ends with the seconds of its phases: its capture (its context
 * build, and the GitHub reads in it with their count), the runs of its profile's tasks, its
 * writes, the rest, and the total.
 *
 * The listing rule (the architect, 2026-11-24T23:52Z, and 2026-11-25T00:52:35Z), so that no one
 * applies it by hand: each task's listing measure is read from the tasks file's build-tasks
 * summary (`<tasks>.summary.json`: a session task's own profile's evidence, a written task's
 * context session's; `taskListings`), and a task with no summary beside its file is not
 * measurable. A task whose rebuilt listing misses more than MAX_LISTING_CHANGE_PERCENT of the
 * names its session's recorded listing holds is dropped before any run, and so is one whose
 * capture builds its listing from the rebuilt profile and whose listing adds more names than that
 * share of them, each side counted alone (score.ts's `listingStanding`), its reason in the
 * summary's task and the counts in its `listing`, by the start of its reason (`listing-misses` or
 * `listing-adds`); it is not run and no verdict reads it. A task whose capture sends its
 * session's recorded system prompt whole (a profile with a `systemPrompt` on Oh My Pi, measured
 * as `session listing`) shows the model the recorded listing whatever the profile installs, so
 * it is not dropped for added names; the summary's `outsideRecordedListing` and a line of the
 * command's output count its runs that read a skill outside that listing, by skill and by how the
 * read was answered (`copy`, an installed copy; `absent`, the unknown-skill reply; score.ts's
 * `outsideListing`), from the reads each run records: a count only, which drops no task and
 * changes no verdict. Claude Code's capture sends no recorded prompt, so its tasks keep both
 * drops. Where the rule drops more than half of the tasks, the command stops before any run and
 * says so with the numbers (exit 3): the architect decides such a load. The skill's verdict is
 * made with and without the tasks whose listing could not be measured (`listedOutcome`), both in
 * the summary's `verdicts`: where they agree it stands, with how many tasks were not measured;
 * where they differ there is no verdict, since it depends on those tasks. Beside the listing
 * counts, the summary's `dispatch` and the listing rule's line count the tasks whose capture
 * carries the Envoy plugin's block of the skill, and of them those that rest on the assumption
 * that their session had Dispatch: a task holds no message of its session, so arms.ts's
 * `dispatchOf` answers from its profile's context alone (a Legion role's, else the assumption),
 * and each capture's record keeps that answer (`dispatch`).
 */
import { createHash } from "node:crypto";
import {
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { stripSkillHeader } from "../skill-scenarios/transcript";
import {
  type Arm,
  dispatchOf,
  type Injection,
  type InstalledCopy,
  PLUGIN_INJECTED,
  pluginInjects,
  withPluginBlock,
} from "./arms";
import {
  anthropicHeaders,
  type CaptureCase,
  type CaptureCheckout,
  type CaptureRecord,
  captureBody,
  capturePaths,
  checkoutMention,
  type HarnessRecord,
  type HarnessRule,
  type HintWriter,
  listingOf,
  type RecordedLine,
  rawBodyFile,
  renamedListingBlocks,
  restoredCapture,
  rewriteBody,
  type SavedHead,
  type SessionFacts,
  type Sourced,
  savedCapture,
  sessionCheckout,
  sessionFacts,
  sessionHeads,
  userTurnKey,
} from "./capture";
import { Case, listedName, Profile, ProfileSkill, type Thinking } from "./cases";
import {
  counted,
  json,
  jsonLines,
  ompSkillName,
  ompText,
  parseJson,
  pastLimit,
  Refused,
  readFrontmatter,
  Stopped,
  schemaFaults,
  textOf,
  timeLimit,
  writeWhole,
} from "./common";
import {
  type AgentDefinition,
  buildContext,
  type ContextRecord,
  cacheKey,
  DAEMON_IMAGE_ITEM,
  daemonCommit,
  installedCopies,
  LOCAL_SKILL_REPO,
  linkTargetPath,
  listingFrontmatter,
  NoSuchFile,
  type RepoSource,
  skillFrontmatter,
  skillTextFrontmatter,
  subagentDefinition,
  UNREADABLE_DIRECTORY,
  UnreadableDirectory,
} from "./context";
import {
  BudgetSpent,
  type ContentBlock,
  deploymentCounts,
  deploymentWords,
  type Gateway,
  type MessagesBody,
  PRICES,
  type RequestBody,
  type Spent,
  SpentRecord,
  type ToolCall,
  toolCalls,
  usdOf,
} from "./gateway";
import { newSeed } from "./judge";
import { type RecordedRequest, startRecorder } from "./recorder";
import {
  AnsweredRead,
  type Boundary,
  type CallPart,
  LeftOutCall,
  LOAD_RULE,
  MAX_SKILL_ROUNDS,
  partTools,
  ReadsPastLastRound,
  type Replayed,
  replayOnce,
  replyBoundary,
  type SkillRead,
  sendOrder,
  UndecidedReply,
} from "./replay";
import {
  descriptionFault,
  LISTING_ADDS,
  LISTING_MEASURE_ITEM,
  LISTING_MISSES,
  type ListingStanding,
  LOAD_RUNS,
  listingStanding,
  MAX_EXTRA_RUNS,
  MAX_LISTING_CHANGE_PERCENT,
  NEAR_MISS_MAX_LOADS,
  NEED_MIN_LOADS,
  type OutsideListing,
  outsideListing,
  outsideListingWords,
  TaskLabel,
  taskFault,
} from "./score";

/** The hosts a load test runs in. */
export const Host = z.enum(["omp", "claude"]);
export type Host = z.infer<typeof Host>;
/** A thinking level, as `omp --thinking` takes it (cases.ts's `Thinking`). */
type ThinkingLevel = z.infer<typeof Thinking>;

/** The model a load test runs unless `--model` says otherwise: the one the pinned hosts' fixture
 *  runs used (testdata/CAPTURE.md, F1 to F3). */
export const LOAD_MODEL = "anthropic/claude-sonnet-5-5";
/** Each host's thinking level unless `--thinking` says otherwise, as those fixture runs set it:
 *  Oh My Pi's `--thinking off` (F1, F2), and Claude Code's own default (F3), `auto` here. */
export const LOAD_THINKING: Readonly<Record<Host, ThinkingLevel>> = { omp: "off", claude: "auto" };
/** The fewest tasks of each label a load test is meant to have. A smaller test still gets its
 *  verdict, with a note that says so. */
const MIN_TASKS_PER_LABEL = 5;

/** The longest one Claude Code run may take: one still running then is killed. */
const RUN_TIMEOUT_MS = 600_000;
/** The longest `claude --version` may run: it took 0.09 s on the pod (Claude Code 2.1.311,
 *  measured 2026-11-23), so a minute is over 600 times that. */
export const CLAUDE_VERSION_TIMEOUT_MS = 60_000;
/** The longest one `claude plugin marketplace add` or `claude plugin install` may run, each of a
 *  local marketplace: they took 0.97 s and 1.20 s on the pod (Claude Code 2.1.311, measured
 *  2026-11-23), so 5 minutes is 250 times the longer one. */
export const CLAUDE_PLUGIN_TIMEOUT_MS = 300_000;
/** What a Claude Code run is given as its provider credential: the recorder reads none, and no
 *  gateway ever gets this one. */
const RECORDER_CREDENTIAL = "not-a-secret-the-recorder-reads-none";
/** Claude Code's line before its skill listing. */
const CLAUDE_LISTING = "The following skills are available for use with the Skill tool:";
/** The arm a load test's capture is: the skill's text as written, the description the test
 *  tests (arms.ts's `old`). */
const ARM = "old";
/** A skill, plugin or marketplace name: what can name a directory and a listing entry. */
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
/** A skill file's frontmatter, its text in group 1. */
const FRONTMATTER = /^---\r?\n([\s\S]*?)\r?\n---(\r?\n|$)/;
/** The levels `claude --effort` takes. */
const CLAUDE_EFFORTS: readonly ThinkingLevel[] = ["low", "medium", "high", "xhigh", "max"];
/** The directory under `--out` that keeps each session head a capture read (capture.ts's
 *  `sessionHeads`), one file per store and path hash. */
const SESSIONS_DIR = "sessions";

const Name = z.string().regex(NAME, { error: "a name of letters, digits, '.', '_' and '-'" });
/** A SHA-256 in hex. */
const Sha256 = z.string().regex(/^[0-9a-f]{64}$/, { error: "a SHA-256 in hex" });

/** The provenance of a load that installs an inventory copy absent from its rebuilt context. */
export const TEST_SETUP_LABEL = "test setup, not observed use";
const TestSetup = z.strictObject({
  label: z.literal(TEST_SETUP_LABEL),
  copy: ProfileSkill,
});
export type TestSetup = z.infer<typeof TestSetup>;
export type SetupLocation = Pick<ProfileSkill, "repo" | "commit" | "path">;

export function setupLocation({ repo, commit, path }: SetupLocation): string {
  return `${repo}@${commit}:${path}`;
}

/** The inventory commit and directory named by `--test-setup`, not a branch or tag. */
export function testSetupOption(value: string): SetupLocation {
  const match = /^([a-z0-9][a-z0-9-]*\/[a-z0-9._-]+)@([a-f0-9]{40}):([^\r\n\\\0]+)$/i.exec(value);
  const [, repo = "", commit = "", dir = ""] = match ?? [];
  if (
    match === null ||
    /\/\.{1,2}$/.test(repo) ||
    (dir !== "." && dir.split("/").some((part) => part === "" || part === "." || part === ".."))
  ) {
    throw new Refused(
      `--test-setup ${JSON.stringify(value)} is not <owner/repo>@<40 hex commit>:<path inside the repository>`
    );
  }
  return { repo, commit: commit.toLowerCase(), path: dir };
}

/** Whether `skill` is the inventory copy, regardless of the name it is installed under. */
export function isSetupCopy(skill: ProfileSkill, at: SetupLocation): boolean {
  return (
    skill.repo.toLowerCase() === at.repo.toLowerCase() &&
    skill.commit.toLowerCase() === at.commit &&
    skill.path === at.path
  );
}

/** Reads the copy's name the same way the inventory does, without recording it as observed. */
export async function resolveTestSetup(
  at: SetupLocation,
  ownName: string,
  read: () => Promise<string | undefined>
): Promise<TestSetup> {
  const where = `--test-setup ${setupLocation(at)}`;
  let text: string | undefined;
  try {
    text = await read();
  } catch (error) {
    throw new Refused(`${where}: cannot read the copy: ${messageFor(error)}`);
  }
  if (text === undefined) throw new Refused(`${where}: cannot read the copy's SKILL.md`);
  const frontmatter = readFrontmatter(text);
  const name = ompSkillName(frontmatter, path.posix.basename(at.path === "." ? at.repo : at.path));
  if (name !== ownName) {
    throw new Refused(`${where}: its skill is named ${name}, not ${ownName} as --skill is`);
  }
  if (listingFrontmatter(frontmatter).hidden) {
    throw new Refused(`${where}: ${name} is hidden from the skill listing`);
  }
  return { label: TEST_SETUP_LABEL, copy: { ...at, name } };
}

/** Materializes the inventory directory for the arm. `--skill` alone does not pin its bytes. */
async function setupDirectory(at: SetupLocation, source: RepoSource, dest: string): Promise<void> {
  let dir = at.path;
  const seen = new Set<string>();
  for (;;) {
    if (seen.has(dir)) throw new Error(`symbolic link loop at ${dir}`);
    seen.add(dir);
    const target = await source.link?.(at.repo, at.commit, dir);
    if (target === undefined) break;
    const next = linkTargetPath(dir, target);
    if (next === undefined) throw new Error(`${dir} links outside the repository to ${target}`);
    dir = next;
  }
  await source.tree(at.repo, at.commit, dir, dest);
}

/** Where a task comes from. */
const TaskSource = z.enum(["session", "written"]);

const TaskFields = {
  id: z.string().min(1),
  text: z.string().refine((text) => text.trim() !== "", "is blank"),
  label: TaskLabel,
  /** The profile file of the session the task came from (loadProfile): absolute, or relative to
   *  the tasks file's directory. A task that names none runs under `--profile`. */
  profile: z.string().min(1).optional(),
  testSetup: TestSetup.optional(),
};
/** One task of a tasks file: a real session's first request, with its store and the hash of its
 *  transcript's path, or a task the reader wrote. */
export const Task = z.discriminatedUnion("source", [
  z.strictObject({
    ...TaskFields,
    source: z.literal("session"),
    store: z.string().min(1),
    pathHash: Sha256,
  }),
  z.strictObject({ ...TaskFields, source: z.literal("written") }),
]);
export type Task = z.infer<typeof Task>;

/** Where Claude Code installs the skills: a local marketplace named `marketplace` whose `plugins`
 *  each hold the profile's skills they name (each but its project skills, `claudeKind`), and
 *  `skillPlugin`, the plugin the tested skill goes in (one of `plugins`, or a plugin of its own). */
const ClaudeLayout = z.strictObject({
  marketplace: Name,
  plugins: z.array(z.strictObject({ name: Name, skills: z.array(Name) })),
  skillPlugin: Name,
});

/** One line of a session's recorded skill listing (`- <name>: <text>`): the name and the text the
 *  session listed the skill under. */
const ListingLine = z.strictObject({ name: z.string().min(1), text: z.string() });

/** The skill listing a task's session recorded, as build.ts keeps it in the task's profile where
 *  the profile's listing measure sets it against one: where it was recorded (`source`: the
 *  session's own prompt, or that of its subagent of the path hash), each listed skill's line in
 *  order, and the entry of the copy of the tested skill's source that the session had (`tested`),
 *  which the profile's skills leave out, since the load test installs the tested text; `tested` is
 *  absent where the profile had no such copy. */
export const RecordedListing = z.strictObject({
  source: z.discriminatedUnion("kind", [
    z.strictObject({ kind: z.literal("session") }),
    z.strictObject({ kind: z.literal("subagent"), pathHash: z.string().min(1) }),
  ]),
  lines: z.array(ListingLine),
  tested: ProfileSkill.optional(),
});
export type RecordedListing = z.infer<typeof RecordedListing>;

/** A load test's profile: what the host installs beside the tested skill, the context it runs
 *  in (a case's; `devbox` unless it names one), and the skill listing its session recorded, where
 *  build.ts kept one (`recordedListing`). */
const LoadProfile = Profile.extend({
  context: Case.shape.context.default("devbox"),
  claude: ClaudeLayout.optional(),
  recordedListing: RecordedListing.optional(),
});
type LoadProfile = z.infer<typeof LoadProfile>;

/** A profile's session repository and the commit its project skills were read at (cases.ts). */
type SessionRepository = NonNullable<Profile["sessionRepository"]>;

/** What a Claude Code capture's record says when the profile names no session repository. */
const NO_SESSION_REPOSITORY =
  "the profile names no session repository, so no skill is placed as a project skill";

/**
 * Where a Claude Code load puts `skill` of `profile`. A project skill is a skill of the profile's
 * `sessionRepository`, at its commit, whose path is `<level>/.claude/skills/<name>` or
 * `<level>/.omp/skills/<name>`, `<level>` the profile's working directory in the checkout or a
 * parent of it up to the checkout's root (build.ts lists the session repository's project skills
 * at those levels). A `.claude/skills` one goes in the checkout (`checkout`), where Claude Code
 * lists it under its bare name; a `.omp/skills` one is left out (`left out`), since Claude Code
 * reads no `.omp/skills`; every other skill goes in the plugin the layout names (`plugin`). A
 * profile that names no session repository has no project skill.
 */
function claudeKind(profile: Profile, skill: ProfileSkill): "checkout" | "left out" | "plugin" {
  const own = profile.sessionRepository;
  if (own === undefined || skill.repo !== own.repo || skill.commit !== own.commit) return "plugin";
  const dir = path.posix.dirname(skill.path);
  const parts = profile.workingDirectory?.split("/") ?? [];
  for (let depth = parts.length; depth >= 0; depth -= 1) {
    const level = parts.slice(0, depth).join("/");
    const under = (kind: string) => (level === "" ? kind : `${level}/${kind}`);
    if (dir === under(".claude/skills")) return "checkout";
    if (dir === under(".omp/skills")) return "left out";
  }
  return "plugin";
}

/** Why the project skill `skill` (`claudeKind`: `checkout` or `left out`) is in no plugin. */
function projectSkillWords(skill: ProfileSkill, kind: "checkout" | "left out"): string {
  return `it is a project skill of ${skill.repo} (${skill.path}), which Claude Code ${kind === "checkout" ? "reads from the checkout" : "does not read"}`;
}

/** Where a Claude Code capture put one skill (`claudeKind`): in a plugin of its marketplace, at
 *  its path in the checkout, or nowhere, and why. */
type ClaudePlacement =
  | { readonly skill: string; readonly as: "plugin"; readonly plugin: string }
  | { readonly skill: string; readonly as: "checkout"; readonly path: string }
  | { readonly skill: string; readonly as: "left out"; readonly reason: string };

/** One tool call of a reply: its name, and its target (an Oh My Pi `read`'s path, a Claude Code
 *  `Skill` call's skill, a `Read`'s path), or null for any other call; and, for an Oh My Pi
 *  `bash` or `write` call, what replay.ts's LOAD_RULE reads of it (`argument`: its command, its
 *  path), absent for any other call and in a run made before it was recorded. */
const ReplyCall = z.strictObject({
  name: z.string(),
  target: z.string().nullable(),
  argument: z.string().optional(),
});

/** One reply of a run (see the file's header): its tool calls in order; the answers the runner
 *  made to its reads (replay.ts's AnsweredRead: the skill, the file inside its directory, `skill`
 *  for a `skill://` read or `path` for a read by path, where the answer came from, and whether it
 *  is an error), null when it holds no tool call or one that reads no skill file; and what it
 *  used. Every reply but a run's last went back with its answers. */
const ReplyRecord = z.strictObject({
  calls: z.array(ReplyCall),
  answers: z.array(AnsweredRead).nullable(),
  usage: SpentRecord,
});
type ReplyRecord = z.infer<typeof ReplyRecord>;

/** What a run came to (see the file's header): `loaded` and `not loaded` runs are judged; for a
 *  run `sent by` another skill, one whose replies read past the last round, or one `undecided`
 *  at a mixed reply, the task runs again. */
const RunOutcome = z.enum([
  "loaded",
  "not loaded",
  "sent by",
  "reads past last round",
  "undecided",
]);

/** What a run does with a reply whose every tool call reads a skill file: on Oh My Pi it answers
 *  it (`answered`), at most MAX_SKILL_ROUNDS times; on Claude Code it ends at its first message
 *  (`first message`), since the runner does not rebuild Claude Code's result of a read. */
const SkillReads = z.enum(["answered", "first message"]);

/** One run of one task: what it came to, the call that loaded the skill (null unless it loaded
 *  it), the skill whose answered text named the tested one (null unless it was sent by one), how
 *  it took skill reads, each reply (ReplyRecord), the calls its continued rounds left out
 *  (replay.ts's LeftOutCall; absent where none was, and from a run made before), why it has no
 *  verdict where it is `undecided` (replay.ts's UndecidedReply, naming each undecided call;
 *  absent otherwise), the last reply's stop reason and request id, what the host lists for the
 *  skill (Oh My Pi's hint, or Claude Code's listed description), the profile file of its task's
 *  context with the SHA-256 of its bytes, the session id each of its requests went with (its
 *  own: `metadata.user_id`, or a Responses body's `prompt_cache_key`), the routing id each went
 *  with (`loadRoutingId`; null for a Responses body, which goes with none), the deployment that
 *  answered each of its requests in order (gateway.ts's `deploymentOf`, "" where the answer
 *  carried none), and the seed of the send order it went in and its place in that order (from
 *  0). A run made before routing ids has neither field. */
export const LoadRun = z.strictObject({
  task: z.string().min(1),
  run: z.int().positive(),
  label: TaskLabel,
  source: TaskSource,
  testSetup: TestSetup.optional(),
  host: Host,
  name: z.string().min(1),
  outcome: RunOutcome,
  loadedBy: z.string().nullable(),
  sentBy: z.string().nullable(),
  skillReads: SkillReads,
  replies: z.array(ReplyRecord).min(1),
  leftOut: z.array(LeftOutCall).optional(),
  reason: z.string().optional(),
  stopReason: z.string().nullable(),
  requestId: z.string().nullable(),
  listed: z.string(),
  profile: z.string().min(1),
  profileDigest: Sha256,
  userId: z.string().nullable(),
  routingId: z.string().nullable().optional(),
  deployments: z.array(z.string()).optional(),
  sendSeed: z.string().min(1),
  sendPlace: z.int().nonnegative(),
});
export type LoadRun = z.infer<typeof LoadRun>;

/** What one run came to, as `loadRun` gives it: the fields of its LoadRun that its replies
 *  give. */
type RunDone = Pick<
  LoadRun,
  | "outcome"
  | "loadedBy"
  | "sentBy"
  | "skillReads"
  | "replies"
  | "leftOut"
  | "reason"
  | "stopReason"
  | "requestId"
  | "userId"
> &
  Required<Pick<LoadRun, "routingId" | "deployments">>;

/** The routing id of run `run` of task `task` under the route seed `seed` (`--route-seed`): the
 *  first 32 hex digits of the SHA-256 of `<seed>:<task>:<run>`, in a UUID's 8-4-4-4-12 groups.
 *  Derived, so every load command of one seed sends the same id for run `run` of `task`. */
export function loadRoutingId(seed: string, task: string, run: number): string {
  const hex = createHash("sha256").update(`${seed}:${task}:${run}`).digest("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20, 32)}`;
}

/** A run of task `task` that failed after one or more of its requests had their replies: why it
 *  failed, and what each of those replies used. Its run file keeps no line of it, since it came to
 *  no outcome and is made again; `runs/<task id>.failed.jsonl` does (`loadFailedRunFile`), so
 *  that the task's and the test's spend count what it used. */
export const FailedRun = z.strictObject({
  task: z.string().min(1),
  testSetup: TestSetup.optional(),
  error: z.string(),
  usage: z.array(SpentRecord).min(1),
});
export type FailedRun = z.infer<typeof FailedRun>;

/** What `loadRun` rejects with when a send of its run fails: the send's error (`cause`), and what
 *  the run's replies before it used, as replay.ts's ReadsPastLastRound keeps a run's spend. */
class RunFailed extends Error {
  constructor(
    cause: unknown,
    readonly usage: readonly Spent[]
  ) {
    super(messageFor(cause), { cause });
  }
}

/** The skill a load test looks for in a reply: its name, and for Claude Code its plugin. */
export interface TestedSkill {
  readonly name: string;
  readonly plugin?: string;
}

/** What an assistant message did: whether it loaded the skill, the call that did (`read
 *  skill://…`, `Skill <plugin>:<name>`, `Read <path>`), and its tool calls' names. */
interface MessageLoad {
  readonly loaded: boolean;
  readonly loadedBy: string | null;
  readonly toolCalls: readonly string[];
}

/** One task's runs so far: how many were made and judged, how many loaded the skill and which,
 *  how many each skill sent, how many read past the last round and how many were undecided at a
 *  mixed reply, how many were made beyond the runs the test asks for, how many to start now,
 *  whether it has its judged runs, why it gives no description evidence (null unless it lacks its
 *  judged runs after its extra runs), and its verdict (score.ts's `taskFault`), null until it has
 *  them or when they are not LOAD_RUNS. */
interface TaskOutcome {
  readonly made: number;
  readonly judged: number;
  readonly loads: number;
  readonly loadedRuns: readonly number[];
  readonly sentBy: Readonly<Record<string, number>>;
  readonly readsPastLastRound: number;
  readonly undecided: number;
  readonly extraRuns: number;
  readonly toStart: number;
  readonly complete: boolean;
  readonly noEvidence: string | null;
  readonly fault: boolean | null;
}

/** One task's dollars so far at list price: its runs', how many runs failed after some of their
 *  requests had replies (FailedRun) and what those replies used, its capture's hints', and all of
 *  them. */
interface TaskSpend {
  readonly runsUsd: number;
  readonly failedRuns: number;
  readonly failedRunsUsd: number;
  readonly hintsUsd: number;
  readonly usd: number;
}

/** The skill's verdict: a description fault (score.ts's `descriptionFault`), none, or no verdict
 *  yet (null); and the reason in words. */
interface LoadVerdict {
  readonly descriptionFault: boolean | null;
  readonly reason: string;
  readonly testSetup?: TestSetup;
}

/** The tasks of `file`. Throws one refusal that names each line it refuses and why (a line
 *  that is not a task, or an id given twice), or that the file holds no task. */
function loadTasks(file: string): Task[] {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`cannot read the tasks file ${file}: ${messageFor(error)}`);
  }
  const tasks: Task[] = [];
  const refusals: string[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    const where = `${file}:${index + 1}`;
    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch (error) {
      refusals.push(`${where} is not JSON: ${messageFor(error)}`);
      continue;
    }
    const parsed = Task.safeParse(raw);
    if (!parsed.success) {
      refusals.push(`${where} is not a task: ${schemaFaults(parsed.error)}`);
      continue;
    }
    if (tasks.some((task) => task.id === parsed.data.id)) {
      refusals.push(`${where}: the task id ${parsed.data.id} is given twice`);
      continue;
    }
    tasks.push(parsed.data);
  }
  if (refusals.length > 0) throw new Refused(`refused tasks:\n${refusals.join("\n")}`);
  if (tasks.length === 0) throw new Refused(`the tasks file ${file} holds no task`);
  return tasks;
}

/** One item of a profile's evidence, as a build-tasks summary keeps it. */
export const EvidenceItem = z.strictObject({ item: z.string(), source: z.string() });
export type EvidenceItem = z.infer<typeof EvidenceItem>;
/** What the load test reads of a tasks file's summary (`<tasks>.summary.json`, as build-tasks and
 *  `build-tasks --reprofile` write it, build.ts): the evidence of each session task's profile, and
 *  of the context session's profile, which each written task takes. */
const TasksSummary = z.looseObject({
  profiles: z
    .looseObject({
      contextSession: z
        .looseObject({ evidence: z.array(EvidenceItem).optional() })
        .nullable()
        .optional(),
      tasks: z.array(
        z.looseObject({ task: z.string(), evidence: z.array(EvidenceItem).optional() })
      ),
    })
    .optional(),
});

/** The listing measure's mark of the names a profile took from its recorded listing (build.ts's
 *  ListingMeasure `fromRecorded`), read loosely as score.ts reads the measure: build.ts imports
 *  this file, so this file does not import it. */
const TakenRead = z.looseObject({ fromRecorded: z.array(z.string()).optional() });

/** Whether `evidence`'s listing measure marks names its profile took from the recorded listing:
 *  the measure cannot see a miss of those names. */
function tookFromRecorded(evidence: readonly EvidenceItem[] | undefined): boolean {
  const item = evidence?.find((each) => each.item === LISTING_MEASURE_ITEM);
  if (item === undefined) return false;
  return (TakenRead.safeParse(parseJson(item.source)).data?.fromRecorded?.length ?? 0) > 0;
}

/** Each task's profile evidence, as the tasks file's summary gives it: a session task's own
 *  profile's, a written task's context session's; undefined with no summary beside the tasks file.
 *  Throws a refusal when the summary is not JSON or no build-tasks summary. */
function summaryEvidence(
  tasksFile: string,
  tasks: readonly Task[]
): Map<string, readonly EvidenceItem[] | undefined> | undefined {
  const file = `${tasksFile}.summary.json`;
  if (!existsSync(file)) return undefined;
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Refused(`the tasks file's summary ${file} is not JSON: ${messageFor(error)}`);
  }
  const summary = TasksSummary.safeParse(raw);
  if (!summary.success) {
    throw new Refused(`${file} is no build-tasks summary: ${schemaFaults(summary.error)}`);
  }
  const profiles = summary.data.profiles;
  const noted = new Map((profiles?.tasks ?? []).map((note) => [note.task, note.evidence]));
  return new Map(
    tasks.map((task) => [
      task.id,
      task.source === "written" ? profiles?.contextSession?.evidence : noted.get(task.id),
    ])
  );
}

/** Each task's standing by the listing measure of its profile (score.ts's `listingStanding`) in
 *  `evidence`, the tasks file's summary's (`summaryEvidence`), and how many tasks' measures mark
 *  names their profile took from the recorded listing (`tookFromRecorded`; `loadTasks` takes no
 *  id twice). With no summary beside the tasks file, each task is not measurable. On Claude Code
 *  no task's capture sends a recorded system prompt (it lists the skills its plugins hold), so
 *  each task's added names count whatever its measure's kind. */
function taskListings(
  tasksFile: string,
  tasks: readonly Task[],
  evidence: ReadonlyMap<string, readonly EvidenceItem[] | undefined> | undefined,
  host: Host
): { readonly standings: Map<string, ListingStanding>; readonly fromRecorded: number } {
  if (evidence === undefined) {
    const why = `the tasks file has no build-tasks summary (${tasksFile}.summary.json) to give its listing measure`;
    const standing: ListingStanding = { state: "not measurable", why };
    return { standings: new Map(tasks.map((task) => [task.id, standing])), fromRecorded: 0 };
  }
  return {
    standings: new Map(
      tasks.map((task) => [
        task.id,
        listingStanding(evidence.get(task.id), { recordedPromptSent: host === "omp" }),
      ])
    ),
    fromRecorded: tasks.filter((task) => tookFromRecorded(evidence.get(task.id))).length,
  };
}

/** A profile file as a load test read it: its absolute path, the SHA-256 of its bytes, its
 *  profile, and the skill the test looks for under it (for Claude Code, in the plugin its layout
 *  puts the tested skill in). */
interface TaskProfile {
  readonly file: string;
  readonly digest: string;
  readonly profile: LoadProfile;
  readonly tested: TestedSkill;
}

/** The profile of `file`, checked for a test of the skill `name` in `host`. Each check reads a
 *  skill's listed name (cases.ts's `listedName`: `<namespace>/<name>` for a namespaced copy).
 *  Throws a refusal naming each fault: a file that is not a profile; a profile skill listed as
 *  `name` (the tested skill is not one of the other skills) or a name listed twice; and for Claude
 *  Code a profile with no plugin layout, a namespaced skill (a layout places skills by their bare
 *  name, and Claude Code has no namespace), or a layout that puts a skill that goes in a plugin
 *  (`claudeKind`) in no plugin or in two, puts a project skill in a plugin, names a skill the
 *  profile does not install, or names a plugin twice. With `setup` (`--test-setup`), it refuses
 *  a profile that installs the inventory copy, unless the task is labelled (`staged`) and the
 *  profile installs the copy once, under the copy's name and no namespace: it then leaves that
 *  entry out, as the arm installs it. */
export function loadProfile(
  file: string,
  host: Host,
  name: string,
  setup?: TestSetup,
  staged = false
): TaskProfile {
  let bytes: Uint8Array;
  let raw: unknown;
  try {
    bytes = readFileSync(file);
    raw = JSON.parse(new TextDecoder().decode(bytes));
  } catch (error) {
    throw new Refused(`cannot read the profile ${file} as JSON: ${messageFor(error)}`);
  }
  const parsed = LoadProfile.safeParse(raw);
  if (!parsed.success) {
    throw new Refused(`the profile ${file} is refused: ${schemaFaults(parsed.error)}`);
  }
  const profile = parsed.data;
  if (setup !== undefined) {
    const installed = profile.skills.filter((skill) => isSetupCopy(skill, setup.copy));
    if (installed.length !== 0) {
      const [entry] = installed;
      if (
        !staged ||
        installed.length !== 1 ||
        entry?.name !== setup.copy.name ||
        entry.namespace !== undefined
      ) {
        throw new Refused(
          `--test-setup ${setupLocation(setup.copy)}: ${file} already installs that copy`
        );
      }
      // The arm installs the staged entry once, under the name tested by this command.
      profile.skills = profile.skills.filter((skill) => skill !== entry);
    }
  }
  const faults: string[] = [];
  const names = profile.skills.map((skill) => listedName(skill));
  if (names.includes(name)) {
    faults.push(`it installs a skill named ${name}, the name the skill is tested under`);
  }
  for (const [index, skill] of names.entries()) {
    if (names.indexOf(skill) !== index) faults.push(`it installs ${skill} twice`);
  }
  const layout = profile.claude;
  if (host === "claude" && layout === undefined) {
    faults.push("it has no `claude` plugin layout, which the Claude Code host needs");
  } else if (host === "claude" && layout !== undefined) {
    const held = layout.plugins.flatMap((plugin) => plugin.skills);
    for (const skill of profile.skills) {
      if (skill.namespace !== undefined) {
        faults.push(
          `it installs ${listedName(skill)}, a second copy of ${skill.name} that Oh My Pi lists under a namespace, and a Claude Code layout places skills by their bare name`
        );
        continue;
      }
      const count = held.filter((each) => each === skill.name).length;
      const kind = claudeKind(profile, skill);
      if (kind === "plugin" && count !== 1) {
        faults.push(`its layout puts ${skill.name} in ${count} plugins, not 1`);
      } else if (kind !== "plugin" && count > 0) {
        faults.push(
          `its layout puts ${skill.name} in a plugin, and ${projectSkillWords(skill, kind)}`
        );
      }
    }
    for (const skill of new Set(held)) {
      if (!names.includes(skill))
        faults.push(`its layout names ${skill}, which it does not install`);
    }
    const plugins = layout.plugins.map((plugin) => plugin.name);
    for (const [index, plugin] of plugins.entries()) {
      if (plugins.indexOf(plugin) !== index) faults.push(`its layout names ${plugin} twice`);
    }
  }
  if (faults.length > 0) {
    throw new Refused(`the profile ${file} is refused: ${faults.join("; ")}`);
  }
  const plugin = host === "claude" ? layout?.skillPlugin : undefined;
  return {
    file,
    digest: createHash("sha256").update(bytes).digest("hex"),
    profile,
    tested: plugin === undefined ? { name } : { name, plugin },
  };
}

/** A task and the profile it runs under. */
interface ProfiledTask {
  readonly task: Task;
  readonly profile: TaskProfile;
}

/** Each task of `tasks` in order, with its profile: the file the task names (absolute, or
 *  relative to the tasks file's directory), else `o.profileFile` (`--profile`); each file read
 *  once (`loadProfile`). Throws a refusal naming each task that names no profile when no
 *  `--profile` is given, or a profile's refusal. */
function taskProfiles(
  tasks: readonly Task[],
  o: Pick<LoadOptions, "tasksFile" | "profileFile" | "host">,
  name: string,
  setup?: TestSetup
): ProfiledTask[] {
  const fallback = o.profileFile === undefined ? undefined : path.resolve(o.profileFile);
  const read = new Map<string, TaskProfile>();
  const profiled: ProfiledTask[] = [];
  const lacking: string[] = [];
  for (const task of tasks) {
    const file =
      task.profile === undefined ? fallback : path.resolve(path.dirname(o.tasksFile), task.profile);
    if (file === undefined) {
      lacking.push(task.id);
      continue;
    }
    const key = setup === undefined ? file : `${file}\0${task.testSetup !== undefined}`;
    let profile = read.get(key);
    if (profile === undefined) {
      profile = loadProfile(file, o.host, name, setup, task.testSetup !== undefined);
      read.set(key, profile);
    }
    profiled.push({ task, profile });
  }
  if (lacking.length > 0) {
    const one = lacking.length === 1;
    throw new Refused(
      `${one ? "task" : "tasks"} ${lacking.join(", ")} of ${o.tasksFile} ${one ? "names" : "name"} no profile, and no --profile is given for a task that names none`
    );
  }
  return profiled;
}

/** What the summary and the report call the source of a Claude Code task's listing line: Claude
 *  Code lists each skill under its description, so no hint is drawn or recorded there. */
const CLAUDE_LISTED = "description (Claude Code)";

/**
 * The lines of a task profile's recorded listing (`listing`; none where it has none) that its Oh
 * My Pi capture lists in place of hints (capture.ts's RecordedLine; the architect,
 * 2026-11-23T07:11Z): each other skill's line as recorded, and the line of the tested skill only
 * where the arm lists it under the name the session listed it under (`tested.name` is
 * `tested.ownName`: a name test's proposed name gets a drawn hint) and with the description the
 * session's copy had (`sessionDescription`, undefined where the profile names no such copy or its
 * SKILL.md cannot be read: a drawn hint then), given with that description so that the capture
 * lists it for that text alone.
 */
export function recordedLines(
  listing: RecordedListing | undefined,
  tested: { readonly name: string; readonly ownName: string; readonly description: string },
  sessionDescription: string | undefined
): RecordedLine[] {
  if (listing === undefined) return [];
  const from =
    listing.source.kind === "session" ? "session" : `subagent ${listing.source.pathHash}`;
  return listing.lines.flatMap(({ name, text }) => {
    if (name !== tested.name && name !== tested.ownName) return [{ name, text, from }];
    return tested.name === tested.ownName &&
      sessionDescription !== undefined &&
      sessionDescription === tested.description
      ? [{ name, text, from, description: sessionDescription }]
      : [];
  });
}

/** Whether `recordedLines` reads the session copy's description for `listing` and `tested`: only
 *  where the listing holds a line of the tested skill's name and the arm lists the skill under
 *  that name. Elsewhere its lines are the same whatever the description. */
function readsSessionDescription(
  listing: RecordedListing | undefined,
  tested: { readonly name: string; readonly ownName: string }
): boolean {
  return (
    listing !== undefined &&
    tested.name === tested.ownName &&
    listing.lines.some((line) => line.name === tested.name)
  );
}

/**
 * Why a load test of the skill `own`, tested under `name`, cannot be made in the context of a
 * profile of `profiled` (see the file's header), or undefined when it can in each: on Claude Code,
 * a skill the Envoy plugin puts into its session; on Oh My Pi, a profile that installs that plugin
 * and a copy under `own` while the skill is tested under another name, and a `subagent:<agent>`
 * profile whose agent autoloads `own`. A profile whose plugins' repositories cannot be read here is
 * left to its tasks' captures, which read the same repositories and report that by their rules.
 */
async function injectedRefusal(
  profiled: readonly ProfiledTask[],
  o: Pick<LoadOptions, "host" | "source" | "cacheDir">,
  own: string,
  name: string
): Promise<string | undefined> {
  if (o.host === "claude" && own === PLUGIN_INJECTED.skill) {
    return `${own} cannot be load tested on Claude Code: Claude Code's Envoy plugin puts it into each session with Dispatch from its session-start hook (packages/claude-envoy/hooks/session-hook.ts), which a load test's Claude Code does not run, and a profile does not say whether its session had that plugin`;
  }
  if (o.host !== "omp") return undefined;
  const faults: string[] = [];
  const read = new Set<string>();
  for (const { profile } of profiled) {
    if (read.has(profile.file)) continue;
    read.add(profile.file);
    const { context } = profile.profile;
    if (
      name !== own &&
      pluginInjects({ skill: own, profile: profile.profile }) &&
      profile.profile.skills.some((skill) => listedName(skill) === own)
    ) {
      faults.push(
        `${profile.file}: it installs ${own} beside the skill tested under ${name}, and the ${PLUGIN_INJECTED.plugin} plugin would put that copy into each request, not the tested text`
      );
    }
    if (!context.startsWith("subagent:")) continue;
    let agent: AgentDefinition | null;
    try {
      agent = await subagentDefinition({ id: profile.file, context, profile: profile.profile }, o);
    } catch {
      // Each task's capture reads the same repositories, and reports the failure by its rules.
      continue;
    }
    if (agent?.autoloadSkills.includes(own) === true) {
      const renamed = name === own ? "" : `, and under the name ${name} it would autoload none`;
      faults.push(
        `${profile.file}: its agent ${agent.name} (${agent.path} of ${agent.repo} at ${agent.commit}) autoloads ${own}, so the subagent gets its text before its task whatever its listing says${renamed}`
      );
    }
  }
  return faults.length === 0
    ? undefined
    : `${own} cannot be load tested in the context of each profile, since a plugin or an agent definition puts it into the session there: ${faults.join("; ")}`;
}

/** The name Oh My Pi gives the skill in `dir` (common.ts's `ompSkillName`: its frontmatter
 *  `name`, trimmed, read as Oh My Pi reads a skill's frontmatter with common.ts's `ompText` and
 *  `readFrontmatter`; else the directory's own name). */
export function skillName(dir: string): string {
  const text = ompText(readFileSync(path.join(dir, "SKILL.md")));
  return ompSkillName(readFrontmatter(text), path.basename(dir));
}

/**
 * A copy of the skill in `dir` under the name `name`, at `<scratch>/<name>`, replacing one there:
 * the same files byte for byte, but for its SKILL.md's frontmatter `name` line, which becomes
 * `name: <name>` (added at the frontmatter's top when it has none). Nothing else that names the
 * skill changes: its own text, its references, and every file outside it; a leading byte order
 * mark, which Oh My Pi drops as it reads the file (`ompText`), stays where it is. Throws a refusal
 * when the SKILL.md has no frontmatter.
 */
export function renamedSkill(dir: string, name: string, scratch: string): string {
  const file = path.join(dir, "SKILL.md");
  const raw = readFileSync(file, "utf8");
  const text = ompText(raw);
  const mark = raw.slice(0, raw.length - text.length);
  const open = /^---\r?\n/.exec(text)?.[0];
  const block = FRONTMATTER.exec(text)?.[1];
  if (open === undefined || block === undefined) {
    throw new Refused(`${file} has no frontmatter, so it has no name to change`);
  }
  const named = /^name:.*$/m.test(block)
    ? block.replace(/^name:.*$/m, `name: ${name}`)
    : `name: ${name}\n${block}`;
  const dest = path.join(scratch, name);
  rmSync(dest, { recursive: true, force: true });
  mkdirSync(scratch, { recursive: true });
  cpSync(dir, dest, { recursive: true, verbatimSymlinks: true });
  writeFileSync(
    path.join(dest, "SKILL.md"),
    `${mark}${open}${named}${text.slice(open.length + block.length)}`
  );
  return dest;
}

/** Whether `target` (a path or a `skill://` URL) reads the SKILL.md of skill `name`, as count.sql
 *  counts a read: a read selector after the last `/` is taken off first. A read of another
 *  source's copy of the name does not: `skill://<namespace>/<name>`, or a path under a namespace's
 *  skills directory (`namespaced/<namespace>/skills/`, where context.ts installs such a copy). */
function readsSkillFile(target: string, name: string): boolean {
  const bare = target.replace(/:[^/]*$/, "");
  if (/\/namespaced\/[^/]+\/skills\/[^/]+\/SKILL\.md$/.test(bare)) return false;
  return (
    bare === `skill://${name}` ||
    bare === `skill://${name}/` ||
    bare === `${name}/SKILL.md` ||
    bare.endsWith(`/${name}/SKILL.md`)
  );
}

/** The string `key` of the tool call `call`'s input, or undefined. */
function inputField(call: ToolCall, key: string): string | undefined {
  const { input } = call;
  const value = typeof input === "object" && input !== null ? Reflect.get(input, key) : null;
  return typeof value === "string" ? value : undefined;
}

/** What the tool call `call` of a reply in `host` reads or names (ReplyCall's target): an Oh My
 *  Pi `read`'s path, a Claude Code `Skill` call's skill, a `Read`'s path; undefined for any other
 *  call. */
function callTarget(host: Host, call: ToolCall): string | undefined {
  if (host === "omp") return call.name === "read" ? inputField(call, "path") : undefined;
  if (call.name === "Skill") return inputField(call, "skill") ?? inputField(call, "command");
  return call.name === "Read" ? inputField(call, "file_path") : undefined;
}

/** The field of an Oh My Pi `bash` or `write` call's input that replay.ts's LOAD_RULE reads
 *  (ReplyCall's `argument`): its command, its path. `ruleArgument` records it by this field, and
 *  `auditLoadMixed` rebuilds the call's input from it. */
const RULE_FIELD = { bash: "command", write: "path" } as const;

/** What replay.ts's LOAD_RULE reads of an Oh My Pi `bash` or `write` call `call` (ReplyCall's
 *  `argument`): its command, its path (RULE_FIELD); undefined for any other call, and on Claude
 *  Code. */
function ruleArgument(host: Host, call: ToolCall): string | undefined {
  if (host !== "omp") return undefined;
  if (call.name === "bash") return inputField(call, RULE_FIELD.bash);
  return call.name === "write" ? inputField(call, RULE_FIELD.write) : undefined;
}

/** How the tool call `call` of a reply loads `skill` in `host` (`read <path>`, `Skill <name>`,
 *  `Read <path>`), or undefined when it does not. */
function loadingCall(host: Host, call: ToolCall, skill: TestedSkill): string | undefined {
  const target = callTarget(host, call);
  if (target === undefined) return undefined;
  if (host === "claude" && call.name === "Skill") {
    const names = [skill.name];
    if (skill.plugin !== undefined) names.push(`${skill.plugin}:${skill.name}`);
    return names.includes(target.replace(/^\/+/, "")) ? `Skill ${target}` : undefined;
  }
  return readsSkillFile(target, skill.name) ? `${call.name} ${target}` : undefined;
}

/** What an assistant message, a reply's `content` in either API, did with `skill` in `host` (see
 *  the file's header): a session's first one for build.ts's control, each reply of a run here. */
export function messageLoad(
  host: Host,
  content: readonly ContentBlock[],
  skill: TestedSkill
): MessageLoad {
  const calls = toolCalls(content);
  const loadedBy = calls.map((call) => loadingCall(host, call, skill)).find((how) => how) ?? null;
  return {
    loaded: loadedBy !== null,
    loadedBy,
    toolCalls: calls.map((call) => call.name ?? ""),
  };
}

/** Whether `text` names the skill `name` (the architect, 2026-11-22T18:59:51Z): it holds
 *  `skill://<name>` or `/skill:<name>`, the name in backticks, or the name beside the word
 *  "skill" ("the x skill", "skill x"). The name alone as a word does not, so that a name such as
 *  `analyze` or `sdd` does not match ordinary prose. */
export function namesSkill(text: string, name: string): boolean {
  const own = literally(name).source;
  // Not part of a longer name: no name character before it, and none after it but a full stop
  // that ends a sentence.
  const before = "(?<![A-Za-z0-9._-])";
  const after = "(?![A-Za-z0-9_-]|\\.[A-Za-z0-9])";
  return new RegExp(
    [
      `skill://${own}${after}`,
      `/skill:${own}${after}`,
      `\`${own}\``,
      `\\b[Ss]kill[ \\t]+${own}${after}`,
      `${before}${own}[ \\t]+[Ss]kill\\b`,
    ].join("|")
  ).test(text);
}

/** The read of the answered round `round` that sends a run of the skill `name` on (`sent by`): a
 *  read of another skill, answered with no error, whose text past its header names `name`
 *  (`namesSkill`); undefined for none. The live run and the mixed audit both end a run by it. */
function senderOf(round: readonly SkillRead[], name: string): SkillRead | undefined {
  return round.find(
    ({ read, text }) =>
      read.skill !== name && !read.isError && namesSkill(stripSkillHeader(text), name)
  );
}

/** Whether load run `run` is judged: it `loaded` the skill or did not (`not loaded`). A run of any
 *  other outcome makes its task run again (`taskOutcome`). */
export function isJudged(run: Pick<LoadRun, "outcome">): boolean {
  return run.outcome === "loaded" || run.outcome === "not loaded";
}

/** The outcome of a task labelled `label` from its `runs`, of the `wanted` judged runs the test
 *  asks for (see the file's header): a run that is not judged makes the task run again, at most
 *  MAX_EXTRA_RUNS times beyond `wanted`. A task that lacks its judged runs once those are made
 *  gives no description evidence, and `noEvidence` says why, with the counts. */
export function taskOutcome(
  label: TaskLabel,
  runs: readonly LoadRun[],
  wanted: number
): TaskOutcome {
  const judged = runs.filter(isJudged);
  const loadedRuns = runs.filter((run) => run.outcome === "loaded").map((run) => run.run);
  const senders = new Map<string, number>();
  for (const { sentBy } of runs) {
    if (sentBy !== null) senders.set(sentBy, (senders.get(sentBy) ?? 0) + 1);
  }
  const readsPastLastRound = runs.filter((run) => run.outcome === "reads past last round").length;
  const undecided = runs.filter((run) => run.outcome === "undecided").length;
  const lacking = wanted - judged.length;
  const left = wanted + MAX_EXTRA_RUNS - runs.length;
  const complete = lacking <= 0;
  const of = `of ${runs.length} runs`;
  const why = [
    ...[...senders].map(([skill, count]) => `sent by ${skill} in ${count} ${of}`),
    ...(readsPastLastRound > 0 ? [`reads past the last round in ${readsPastLastRound} ${of}`] : []),
    ...(undecided > 0 ? [`undecided at a mixed reply in ${undecided} ${of}`] : []),
  ];
  return {
    made: runs.length,
    judged: judged.length,
    loads: loadedRuns.length,
    loadedRuns,
    sentBy: Object.fromEntries(senders),
    readsPastLastRound,
    undecided,
    extraRuns: Math.max(0, runs.length - wanted),
    toStart: complete ? 0 : Math.max(0, Math.min(lacking, left)),
    complete,
    noEvidence:
      complete || left > 0
        ? null
        : `${judged.length} of ${wanted} judged runs after ${MAX_EXTRA_RUNS} extra runs: ${why.join("; ")}`,
    fault: complete && wanted === LOAD_RUNS ? taskFault({ label, loads: loadedRuns.length }) : null,
  };
}

/** The skill's verdict from its tasks' outcomes, of `wanted` judged runs a task: none yet while a
 *  task can still make runs; else from the tasks that have their judged runs, a task that gives no
 *  description evidence left out and named, and none when no task gives any. */
export function skillOutcome(
  tasks: readonly {
    readonly id: string;
    readonly label: TaskLabel;
    readonly outcome: TaskOutcome;
  }[],
  wanted: number
): LoadVerdict {
  const lacking = tasks
    .filter((task) => !task.outcome.complete && task.outcome.noEvidence === null)
    .map((task) => task.id);
  if (lacking.length > 0) {
    return {
      descriptionFault: null,
      reason: `no verdict yet: tasks ${lacking.join(", ")} lack runs`,
    };
  }
  if (wanted !== LOAD_RUNS) {
    return {
      descriptionFault: null,
      reason: `no verdict: the load test's rule takes ${LOAD_RUNS} runs a task, and this test runs ${wanted}`,
    };
  }
  const evidence = tasks.filter((task) => task.outcome.complete);
  const without = tasks.filter((task) => !task.outcome.complete).map((task) => task.id);
  const lack = `lack their ${wanted} judged runs after ${MAX_EXTRA_RUNS} extra runs`;
  if (evidence.length === 0) {
    return {
      descriptionFault: null,
      reason: `no verdict: no task gives description evidence (tasks ${without.join(", ")} ${lack})`,
    };
  }
  const fault = descriptionFault(
    evidence.map((task) => ({ label: task.label, loads: task.outcome.loads }))
  );
  const faulty = evidence.filter((task) => task.outcome.fault).map((task) => task.id);
  const left =
    without.length === 0
      ? ""
      : `; tasks ${without.join(", ")} give no description evidence: they ${lack}`;
  return {
    descriptionFault: fault,
    reason: `${fault ? `a description fault: tasks ${faulty.join(", ")}` : `no description fault in ${evidence.length} tasks`}${left}`,
  };
}

/** The skill's verdict under the listing rule (the architect, 2026-11-22T03:04Z): made twice, from
 *  all `tasks` (`withUnmeasured`) and from those whose listing was measured
 *  (`withoutUnmeasured`, no verdict when none was). Where no task is unmeasured, or the first is
 *  no verdict yet, it is the first; where the two agree, the first, its reason giving how many
 *  tasks were not measured; where they differ, no verdict, its reason giving both. */
function listedOutcome(
  tasks: readonly {
    readonly id: string;
    readonly label: TaskLabel;
    readonly outcome: TaskOutcome;
    readonly measured: boolean;
  }[],
  wanted: number
): { verdict: LoadVerdict; withUnmeasured: LoadVerdict; withoutUnmeasured: LoadVerdict } {
  const withUnmeasured = skillOutcome(tasks, wanted);
  const measured = tasks.filter((task) => task.measured);
  const withoutUnmeasured: LoadVerdict =
    measured.length === 0
      ? { descriptionFault: null, reason: "no verdict: no task's listing was measured" }
      : skillOutcome(measured, wanted);
  const unmeasured = tasks.length - measured.length;
  const both = { withUnmeasured, withoutUnmeasured };
  if (unmeasured === 0 || withUnmeasured.descriptionFault === null) {
    return { verdict: withUnmeasured, ...both };
  }
  const count = `${unmeasured} of its ${tasks.length} tasks`;
  if (withoutUnmeasured.descriptionFault === withUnmeasured.descriptionFault) {
    return {
      verdict: {
        descriptionFault: withUnmeasured.descriptionFault,
        reason: `${withUnmeasured.reason}; the listing of ${count} could not be measured, and without them the verdict is the same`,
      },
      ...both,
    };
  }
  return {
    verdict: {
      descriptionFault: null,
      reason: `no verdict: depends on tasks whose listing could not be measured (${count}): with them, ${withUnmeasured.reason}; without them, ${withoutUnmeasured.reason}`,
    },
    ...both,
  };
}

/** Every text of a request body: its system blocks, and each message's text parts. */
function bodyTexts(body: unknown): string[] {
  const texts: string[] = [];
  const take = (content: unknown): void => {
    if (typeof content === "string") texts.push(content);
    if (!Array.isArray(content)) return;
    for (const part of content) {
      const text = typeof part === "object" && part !== null ? Reflect.get(part, "text") : null;
      if (typeof text === "string") texts.push(text);
    }
  };
  if (typeof body !== "object" || body === null) return texts;
  take(Reflect.get(body, "system"));
  const messages: unknown = Reflect.get(body, "messages");
  for (const message of Array.isArray(messages) ? messages : []) {
    take(typeof message === "object" && message !== null ? Reflect.get(message, "content") : null);
  }
  return texts;
}

/** The skills a Claude Code request body lists for its Skill tool, each name (`<plugin>:<name>`
 *  for a plugin's skill) with its listed description, in order. A description's further lines,
 *  up to the next entry, are kept with it. */
export function claudeListing(body: unknown): Map<string, string> {
  const listing = new Map<string, string>();
  for (const text of bodyTexts(body)) {
    const start = text.indexOf(CLAUDE_LISTING);
    if (start < 0) continue;
    let last: string | undefined;
    const lines = text
      .slice(start + CLAUDE_LISTING.length)
      .replace(/^\n+/, "")
      .split("\n");
    for (const line of lines) {
      if (line.trim() === "") break;
      const entry = /^- ([^\s:]+(?::[^\s:]+)?): (.*)$/.exec(line);
      if (entry?.[1] !== undefined && entry[2] !== undefined) {
        last = entry[1];
        listing.set(last, entry[2]);
      } else if (last !== undefined) {
        listing.set(last, `${listing.get(last)}\n${line}`);
      }
    }
  }
  return listing;
}

/** What a Claude Code capture's `OS Version:` line shows: the system alone, the same for every
 *  task, where the store records no value of the session's. */
const CLAUDE_NEUTRAL_OS = "Linux";

/** `dir` as Claude Code names its project directory under `<config>/projects/`: each character
 *  that is no letter or digit a `-`. */
function claudeProjectName(dir: string): string {
  return dir.replace(/[^A-Za-z0-9]/g, "-");
}

/** `text` as a regular expression that matches it alone, everywhere. */
function literally(text: string): RegExp {
  return new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g");
}

/**
 * The harness rules of a Claude Code capture (capture.ts, "The harness strings") that ran in
 * `workspace` with its config directory at `configDir`, for the session `facts`: its memory
 * directory's project path (`<config>/projects/<the working directory, its non-alphanumerics as
 * -s>`) names the session's config directory and working directory; its `Today's date is …`
 * line the session's date; its `OS Version:` line CLAUDE_NEUTRAL_OS; and every other mention of
 * the config directory or the working directory the session's. When the working directory is
 * below the root of the context's `checkout` (context.ts), every other mention of that checkout,
 * a parent's `CLAUDE.md` among them, names the session's checkout (capture.ts's
 * `sessionCheckout`).
 */
export function claudeHarnessRules(
  workspace: string,
  configDir: string,
  facts: SessionFacts,
  checkout?: CaptureCheckout
): HarnessRule[] {
  const config = facts.claudeConfigDir.value;
  const cwd = facts.cwd.value;
  return [
    ...(configDir === ""
      ? []
      : [
          {
            what: "the memory directory's project path",
            pattern: literally(`${configDir}/projects/${claudeProjectName(workspace)}`),
            to: () => `${config}/projects/${claudeProjectName(cwd)}`,
          },
        ]),
    {
      what: "the date line",
      pattern: /Today's date is \d{4}-\d{2}-\d{2}\./g,
      to: () => `Today's date is ${facts.date.value}.`,
    },
    {
      what: "the OS version line",
      pattern: /( - OS Version: )[^\n]*/g,
      to: (_match, lead = "") => `${lead}${CLAUDE_NEUTRAL_OS}`,
    },
    ...(configDir === ""
      ? []
      : [{ what: "the config directory", pattern: literally(configDir), to: () => config }]),
    { what: "the working directory", pattern: literally(workspace), to: () => cwd },
    ...(checkout === undefined
      ? []
      : [
          {
            what: "the checkout",
            ...checkoutMention(checkout.dir, sessionCheckout(cwd, checkout.workingDirectory)),
          },
        ]),
  ];
}

/** The files of the capture of task `taskId` in `host` under `out`. */
export function loadCapturePaths(
  out: string,
  host: Host,
  taskId: string
): { readonly body: string; readonly record: string } {
  const dir = path.join(out, "captures");
  if (host === "omp") return capturePaths(dir, taskId, ARM);
  const stem = path.join(dir, `${encodeURIComponent(taskId)}.claude`);
  return { body: `${stem}.body.json`, record: `${stem}.capture.json` };
}

/** The run file of task `taskId` under `out`. */
export function loadRunFile(out: string, taskId: string): string {
  return path.join(out, "runs", `${encodeURIComponent(taskId)}.jsonl`);
}

/** The file of task `taskId`'s failed runs under `out` (FailedRun). */
export function loadFailedRunFile(out: string, taskId: string): string {
  return path.join(out, "runs", `${encodeURIComponent(taskId)}.failed.jsonl`);
}

/** What a Claude Code capture used and saw. */
interface ClaudeCaptureRecord {
  readonly task: string;
  readonly capturedAt: string;
  readonly claude: { readonly binary: string; readonly version: string };
  readonly model: string;
  readonly thinking: ThinkingLevel;
  readonly skill: TestedSkill;
  /** The profile file the capture's context was built from, and the SHA-256 of its bytes. */
  readonly profile: string;
  readonly profileDigest: string;
  /** The request's path, and the `anthropic-*` headers Claude Code sent with it, which each run
   *  sends with the body. */
  readonly requestPath: string;
  readonly requestHeaders: Readonly<Record<string, string>>;
  /** The tested skill's listed description, where that line came from (`listedSource`: its
   *  description, since Claude Code lists no hint), and every skill the body lists. */
  readonly listed: string;
  readonly listedSource: typeof CLAUDE_LISTED;
  readonly listing: readonly { readonly name: string; readonly description: string }[];
  /** The other requests the run sent, each answered by the recorder. */
  readonly otherRequests: readonly { readonly path: string; readonly bytes: number }[];
  readonly context: ContextRecord;
  /** The profile's user-level context file Claude Code read as user memory (`CLAUDE.md` in its
   *  config directory), or null when the profile has none. */
  readonly userMemory: UserMemory | null;
  /** The profile's session repository, whose project skills the capture placed as Claude Code
   *  reads them (`claudeKind`), or why it placed none; and where it put each skill of the
   *  profile, then the tested one. */
  readonly sessionRepository: SessionRepository | { readonly none: string };
  readonly placements: readonly ClaudePlacement[];
  readonly runMs: number;
  /** The harness-string rule's record (capture.ts's HarnessRecord). */
  readonly harness: HarnessRecord;
}

/** A task's captured body, the headers it goes with, what the host lists for the skill, the
 *  milliseconds its context build took (0 when the capture was read back, or the context was
 *  built for an earlier task), the arm its runs answer the tested skill's reads from, and the
 *  installed copies of its profile's other skills, which they answer a read of one from
 *  (context.ts's `installedCopies`; none on Claude Code, whose runs answer no read). */
interface Captured {
  readonly body: RequestBody;
  readonly headers: Readonly<Record<string, string>>;
  readonly listed: string;
  readonly contextMs: number;
  readonly arm: Arm;
  readonly copies: readonly InstalledCopy[];
}

/** The scratch Claude Code home of one test: the environment each run gets, the working
 *  directory, the checkout it is in when it is below the checkout's root, the appended system
 *  prompt, Claude Code's version, the context's record, the user memory file written, where each
 *  skill went, and the milliseconds its context build took. */
interface ClaudeSetup {
  readonly env: Record<string, string>;
  readonly workspace: string;
  readonly checkout: CaptureCheckout | undefined;
  readonly prompt: string | undefined;
  readonly version: string;
  readonly context: ContextRecord;
  readonly userMemory: UserMemory | null;
  readonly placements: readonly ClaudePlacement[];
  readonly contextMs: number;
}

/** The context file of a profile that Claude Code reads as user memory: where it comes from. */
interface UserMemory {
  readonly repo: string;
  readonly commit: string;
  readonly path: string;
}

/** The milliseconds each phase of a task took: its capture (made or read back), the context
 *  build within it, the GitHub reads made in it (how many, and the milliseconds one ran), the
 *  runs of its profile's tasks, sent together, and the writes of its run file and the summary. */
interface TaskPhases {
  readonly captureMs: number;
  readonly contextMs: number;
  readonly github: { readonly calls: number; readonly ms: number };
  readonly runsMs: number;
  writeMs: number;
}

/** A task whose capture is made, ready for its runs: the task, its profile, its capture, and its
 *  capture's phases. */
interface ReadyTask {
  readonly task: Task;
  readonly profile: TaskProfile;
  readonly captured: Captured;
  readonly captureMs: number;
  readonly github: { readonly calls: number; readonly ms: number };
}

/** What a load test reads and writes beside its inputs. */
interface LoadIo {
  /** Writes `text` to standard output as it is. */
  out(text: string): void;
  /** Writes one line to standard error. */
  err(line: string): void;
  /** The process's peak resident memory so far, in bytes. */
  peakRssBytes(): number;
}

interface LoadOptions {
  readonly io: LoadIo;
  readonly host: Host;
  /** The skill's directory as written. */
  readonly skillDir: string;
  /** A digest of that directory's files, which the settings of `--out` hold. */
  readonly skillDigest: string;
  /** The name it is tested under, when not its own. */
  readonly name?: string;
  readonly tasksFile: string;
  /** The profile of each task that names none (`--profile`), when one is given. */
  readonly profileFile?: string;
  readonly runs: number;
  readonly out: string;
  /** The model (`anthropic/<id>`, or `<id>`) and the thinking level. */
  readonly model: string;
  readonly thinking: ThinkingLevel;
  /** Where the runs go: counted and logged. */
  readonly gateway: Gateway;
  /** Writes Oh My Pi's hints (capture.ts's `gatewayHints`). */
  readonly hints: HintWriter;
  /** Where the profile's skills, plugins and context files are read. */
  readonly source: RepoSource;
  /** The scratch directory, and the cache of context.ts's builds and of the hints. */
  readonly scratch: string;
  readonly cacheDir: string;
  /** Oh My Pi: its binary (only where a capture is made: none is given when every one is read
   *  back); the `models.yml` a capture needs (a missing capture fails without it); the model that
   *  writes hints; the draw of its hints (`--hint-draw`; capture.ts's header, "The draw");
   *  settings files laid over the scratch profile's. */
  readonly omp?: string;
  readonly models?: string;
  readonly hintModel?: string;
  readonly hintDraw: number;
  readonly configFiles: readonly string[];
  /** Claude Code's binary. */
  readonly claude: string;
  /** The request log, named in the summary. */
  readonly requestLog: string;
  /** The most the peak resident memory may reach: once a profile's tasks are captured and their
   *  runs sent and saved, a peak above it stops the test after those tasks. */
  readonly memoryLimitBytes: number;
  /** The store command (config.ts's `psqlCommand`) and the stores it may read: a capture reads
   *  the head of its task's session through it (capture.ts's `sessionHeads`). */
  readonly psqlCommand: string;
  readonly stores: readonly string[];
  /** The dev box's CLAUDE_CONFIG_DIR (config.ts's `devboxClaudeConfigDir`), when the
   *  configuration names one. */
  readonly devboxClaudeConfigDir?: string;
  /** The session a written task stands for (`--context-session`): the session whose context
   *  its profile was rebuilt from. */
  readonly contextSession?: { readonly store: string; readonly pathHash: string };
  /** The inventory copy to install as test setup, when given. */
  readonly testSetup?: SetupLocation;
  /** The seed each run's routing id is derived from (`--route-seed`, `loadRoutingId`); empty
   *  when not given. */
  readonly routeSeed?: string;
}

/** What the test came to: each task or run that failed. */
interface LoadResult {
  readonly failures: readonly string[];
}

/** How a test's runs are sent, as its settings record it: each run with a session id of its own,
 *  and the runs of a profile's tasks in one seeded order. An `--out` whose settings do not record
 *  it holds runs that went with their capture's one id, task after task, so it is refused. */
const SENDS = "each run its own session id; a profile's tasks interleaved in one seeded order";

/** How a test's runs take skill reads, as its settings record it (see the file's header). An
 *  `--out` whose settings do not record it holds runs that stopped at their first message, or
 *  that ended at a reply that read skill files beside other calls before replay.ts's LOAD_RULE
 *  sorted them, so it is refused. */
const SKILL_READS = `Oh My Pi answers a reply that only reads skill files, or reads them beside calls that only read or look (the others left out), at most ${MAX_SKILL_ROUNDS} rounds; a run sent by another skill, still reading after them, or undecided at a reply that reads them beside a call that is neither runs again, at most ${MAX_EXTRA_RUNS} extra runs a task`;

/** What an Oh My Pi test lists each skill under, as its settings record it with its draw (see the
 *  file's header). An `--out` whose settings do not record it holds captures whose every skill
 *  was listed under a drawn hint, so it is refused. */
const LISTED_LINES =
  "each skill under the line its session recorded where the arm lists it under that name and text, else under a hint of the test's draw";

/** The settings of a test that an `--out` holds: a test of other settings is refused there.
 *  `hints` is absent on Claude Code, which lists descriptions. */
interface Settings {
  readonly skillText: string;
  readonly name: string;
  readonly renamedFrom: string | null;
  readonly host: Host;
  readonly model: string;
  readonly thinking: ThinkingLevel;
  readonly runs: number;
  readonly hintModel: string | null;
  readonly hints?: { readonly listed: typeof LISTED_LINES; readonly draw: number };
  readonly sends: typeof SENDS;
  readonly skillReads: typeof SKILL_READS;
  readonly testSetup?: TestSetup;
}

/** A test's summary, as `summary.json` holds it. Only what a later command checks is read back:
 *  each task's text and profile digest, and why it was dropped. */
const SavedSummary = z.looseObject({
  settings: z.unknown(),
  tasks: z.array(
    z.looseObject({
      id: z.string(),
      text: z.string(),
      profileDigest: z.string().optional(),
      dropped: z.string().nullable().optional(),
    })
  ),
});

/** The role prompt a saved capture's record says its context composed (context.ts's
 *  ContextRecord `rolePrompt`, in capture.ts's record and in the Claude Code one): the plugin
 *  repository and the commit Compose ran at, and whether that was the daemon image's commit. A
 *  record made before `commitOf` was recorded composed it at the plugin's commit. A record whose
 *  context composed none (no Legion role, or the case's own text) does not parse. */
const SavedRolePrompt = z.looseObject({
  context: z.looseObject({
    rolePrompt: z.looseObject({
      repo: z.string(),
      commit: z.string(),
      commitOf: z.enum(["daemon image", "plugin"]).default("plugin"),
    }),
  }),
});

/** Why the capture whose record is `record` is not the capture of a task whose evidence's daemon
 *  image items are `evidence`: its context composed the role prompt at another commit than that
 *  evidence names for the repository it composed it from (context.ts's `daemonCommit`; the
 *  plugin's commit where it names none). Undefined when it is, when no record is saved there, and
 *  when its context composed no role prompt. */
function rolePromptFault(record: string, evidence: readonly EvidenceItem[]): string | undefined {
  if (!existsSync(record)) return undefined;
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(record, "utf8"));
  } catch {
    // Not this check's to refuse: the load's `readCapture`, which reads every task's record
    // before any capture step, then stops the command on it with `JSON.parse`'s error.
    return undefined;
  }
  const made = SavedRolePrompt.safeParse(raw).data?.context.rolePrompt;
  if (made === undefined) return undefined;
  const madeAt = made.commitOf === "daemon image" ? made.commit : undefined;
  const wanted = daemonCommit({ evidence }, made.repo);
  if (madeAt === wanted) return undefined;
  const was =
    madeAt === undefined
      ? `at the plugin's commit ${made.commit} of ${made.repo}`
      : `at ${madeAt}, the daemon image's commit of ${made.repo}`;
  const now =
    wanted === undefined
      ? `names no daemon image commit of ${made.repo}, so its role prompt is composed at the plugin's commit`
      : `names ${wanted} as the daemon image's commit of ${made.repo}`;
  return `capture's role prompt was composed ${was} (${record}), and its evidence in the tasks file's summary now ${now}`;
}

/** Runs `argv` to its end with `env` and returns its standard output; throws, naming it and its
 *  last output lines, when it fails, and naming it and the limit (common.ts's `pastLimit`) when
 *  it runs past `timeoutMs`, at which it is ended (`timeLimit`). Exported for load.test.ts. */
export async function mustRun(
  argv: readonly string[],
  env: Record<string, string>,
  timeoutMs: number
): Promise<string> {
  const child = Bun.spawn([...argv], { env, stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const limit = timeLimit(child, timeoutMs);
  const [stdout, stderr, code] = await Promise.all([
    textOf(child.stdout, limit),
    textOf(child.stderr, limit),
    child.exited,
  ]);
  limit.clear();
  if (limit.timedOut()) throw new Error(pastLimit(argv.join(" "), timeoutMs));
  if (code !== 0) {
    const tail = `${stdout}\n${stderr}`.trim().split("\n").slice(-12).join("\n");
    throw new Error(`\`${argv.join(" ")}\` failed with exit ${code}: ${tail}`);
  }
  return stdout;
}

/** The dollars the hint calls of an Oh My Pi capture's record cost (each call's `usage` is
 *  gateway.ts's `Spent`). */
function hintUsd(record: CaptureRecord): number {
  return record.hintCalls.reduce((sum, call) => {
    const usd: unknown =
      typeof call.usage === "object" && call.usage !== null ? Reflect.get(call.usage, "usd") : 0;
    return sum + (typeof usd === "number" ? usd : 0);
  }, 0);
}

/** The line a task's capture lists the tested skill under, and where it came from: capture.ts's
 *  HintSource for Oh My Pi, CLAUDE_LISTED for Claude Code. */
interface ListedUnder {
  readonly source: string;
  readonly line: string;
}

/** What a task's summary says of the source of a line whose Oh My Pi capture record names none. */
const NO_SOURCE = "not recorded: the capture was made before its record named each line's source";

/** What the summary, and a task with saved runs (`heldCaptureFault`), read of a saved capture's
 *  record: the line a Claude Code record lists the tested skill under, each listed skill's line of
 *  an Oh My Pi record with its source (capture.ts's CaptureRecord `hints`), and the line an Oh My
 *  Pi record lists each skill under (its `listing`, which `ompCapture` reads the tested skill's
 *  from; a Claude Code record's entries give a description in its place). */
const SavedListed = z.looseObject({
  listed: z.string().optional(),
  hints: z
    .array(z.looseObject({ skill: z.string(), hint: z.string(), source: z.string().optional() }))
    .optional(),
  listing: z.array(z.looseObject({ name: z.string(), hint: z.string().optional() })).optional(),
});

/** Why task `taskId`, whose run file under `out` holds `runs`, may not take its saved capture on
 *  `host` for its further runs, or undefined. Those runs replayed that capture, and a run made now
 *  must replay it too, never one made again: so both its files must be there, and, since a run
 *  records no digest of its capture's body, the capture must list the tested skill (`name`) under
 *  the line each of those runs records it was listed under (LoadRun's `listed`, the one value a
 *  run takes from its capture; the profile a capture or run was made under is checked apart). */
function heldCaptureFault(
  out: string,
  host: Host,
  taskId: string,
  name: string,
  runs: readonly LoadRun[]
): string | undefined {
  const files = loadCapturePaths(out, host, taskId);
  const lines = [...new Set(runs.map((run) => JSON.stringify(run.listed)))].join(" and ");
  const held = `task ${taskId}'s run file ${loadRunFile(out, taskId)} holds ${counted(runs.length, "run")}, whose capture ${files.body} listed ${name} under ${lines}`;
  const absent = [files.body, files.record].filter((file) => !existsSync(file));
  if (absent.length > 0) {
    return `${held}; ${absent.join(" and ")} ${absent.length === 1 ? "is" : "are"} not there`;
  }
  let saved: z.infer<typeof SavedListed>;
  try {
    saved = SavedListed.parse(JSON.parse(readFileSync(files.record, "utf8")));
  } catch (error) {
    return `${held}; its record ${files.record} cannot be read: ${messageFor(error)}`;
  }
  const listed =
    host === "omp" ? saved.listing?.find((entry) => entry.name === name)?.hint : saved.listed;
  if (runs.every((run) => run.listed === listed)) return undefined;
  return `${held}; the capture there lists it under ${listed === undefined ? "no line" : JSON.stringify(listed)}`;
}

/** Adds only the tested line of `name` to the recorded prompt `blocks` when it was absent. The
 *  capture substitutes the drawn hint using its normal recorded-prompt rule. */
function setupPrompt(blocks: string[], name: string): string[] {
  if (listingOf(blocks).has(name)) return blocks;
  const listing = /(<skills>\n[\s\S]*?)<\/skills>/;
  const index = blocks.findIndex((block) => listing.test(block));
  const line = `- ${name}: \n`;
  if (index === -1) return [...blocks, `<skills>\n${line}</skills>`];
  return blocks.map((block, at) =>
    at === index ? block.replace(listing, `$1${line}</skills>`) : block
  );
}

/** One load test, as `runLoad` runs it: its options, settings, scratch root, the tested skill's
 *  directory, and each task's daemon image item of its summary evidence, which its capture reads. */
class LoadTest {
  /** Claude Code's scratch home of each profile, by the SHA-256 of the profile's file. */
  private readonly claudeSetups = new Map<string, Promise<ClaudeSetup>>();
  /** The description of the tested skill's copy each profile's session had, by the profile's
   *  file (`sessionDescription`). */
  private readonly sessionDescriptions = new Map<string, Promise<string | undefined>>();

  constructor(
    readonly o: LoadOptions,
    readonly settings: Settings,
    readonly root: string,
    readonly skillDir: string,
    readonly daemonImages: ReadonlyMap<string, readonly EvidenceItem[]>
  ) {}

  say(line: string): void {
    this.o.io.out(`load: ${line}\n`);
  }

  /** The description of the tested skill's copy that the session of `profile` had (its recorded
   *  listing's `tested`), read as Oh My Pi reads a skill's frontmatter from its repository at its
   *  commit, once per profile file; undefined where the profile names no such copy, or that
   *  commit holds no SKILL.md there. */
  sessionDescription(profile: TaskProfile): Promise<string | undefined> {
    const made = this.sessionDescriptions.get(profile.file);
    if (made !== undefined) return made;
    const copy = profile.profile.recordedListing?.tested;
    const read = (async () => {
      if (copy === undefined) return undefined;
      let bytes: Uint8Array;
      try {
        bytes = await this.o.source.file(
          copy.repo,
          copy.commit,
          path.posix.join(copy.path, "SKILL.md")
        );
      } catch (error) {
        if (error instanceof NoSuchFile) return undefined;
        throw error;
      }
      return skillTextFrontmatter(ompText(bytes)).description;
    })();
    this.sessionDescriptions.set(profile.file, read);
    return read;
  }

  /** The recorded prompt `blocks` as the case sends it: under `--test-setup` with the tested line
   *  added where it was absent (`setupPrompt`), for a skill tested under another name with its
   *  listing line renamed (`renamedListingBlocks`), else as recorded. */
  private casePrompt(blocks: string[]): string[] {
    const { name, renamedFrom, testSetup } = this.settings;
    if (testSetup !== undefined) return setupPrompt(blocks, name);
    if (renamedFrom === null) return blocks;
    return renamedListingBlocks(blocks, renamedFrom, name);
  }

  /** The case a capture of `id` reads in the context of `profile`, and the arm it installs: the
   *  profile's skills and the tested skill from its directory, with `text` as the one message,
   *  and the rest of the profile as it is (its plugins, context files, appended prompt, and a
   *  subagent's recorded prompt and tools), but that a recorded prompt of a skill tested under
   *  another name lists it under that name (`renamedListingBlocks`), as the scratch copy's
   *  frontmatter and directory name it. The tested skill's entry names the local directory under
   *  context.ts's LOCAL_SKILL_REPO: context.ts installs an arm's own skill from `skillDir` and
   *  reads no repository for it. The case's evidence is `evidence`: a Legion task's daemon image
   *  item, at whose commit context.ts composes its role prompt. */
  caseOf(
    id: string,
    text: string,
    profile: LoadProfile,
    evidence: readonly EvidenceItem[] = []
  ): { c: CaptureCase; arm: Arm } {
    const { name, renamedFrom, model, thinking } = this.settings;
    const ownName = renamedFrom ?? name;
    const own: ProfileSkill = {
      repo: LOCAL_SKILL_REPO,
      commit: this.settings.skillText,
      path: this.skillDir,
      name,
    };
    // The recorded listing goes to the capture as lines (`recordedLines`), not in the profile.
    const { context, claude: _claude, systemPrompt, recordedListing: _listing, ...rest } = profile;
    const recorded = systemPrompt === undefined ? undefined : this.casePrompt(systemPrompt);
    const caseProfile: Profile = {
      ...rest,
      ...(recorded === undefined ? {} : { systemPrompt: recorded }),
      skills: [...profile.skills, own],
    };
    const task = {
      role: "user",
      content: [{ type: "text", text }],
      attribution: "user",
      timestamp: Date.now(),
    };
    // The Envoy plugin's block of the tested text before the task's, under every name, where the
    // profile installs the plugin (see the file's header).
    const placed =
      this.o.host === "omp" && pluginInjects({ skill: ownName, profile })
        ? withPluginBlock([task], this.skillDir)
        : undefined;
    const injections: Injection[] =
      placed === undefined
        ? []
        : [{ by: "plugin", plugin: PLUGIN_INJECTED.plugin, name: ownName, at: placed.at }];
    return {
      c: {
        id,
        skill: name,
        model,
        thinking,
        context,
        profile: caseProfile,
        sessionDigest: null,
        ...(evidence.length === 0 ? {} : { evidence: [...evidence] }),
      },
      arm: {
        name: ARM,
        skill: name,
        skillDir: this.skillDir,
        installedDir: path.join("/skills", name),
        messages: placed?.messages ?? [task],
        profileSkills: caseProfile.skills,
        injections,
        // A task holds no message of its session, so the answer comes from its context alone: a
        // Legion role's, or the assumption.
        dispatch: dispatchOf({ context, messages: [] }),
      },
    };
  }

  /** What a capture of `task` shows of the session it stands for (capture.ts's `sessionFacts`):
   *  the head of the task's own session, or for a written task of `--context-session`, read once
   *  from its store and saved under `<out>/sessions/`; the reminder Oh My Pi journaled for that
   *  session's first user turn, else one rendered for that turn's UTC date (the session's start's
   *  when its head holds no user turn). A written task with no `--context-session` names no
   *  session, so its values are neutral and its date the capture's own. Throws when the store is
   *  not the configuration's, or the head cannot be read. */
  async sessionOf(task: Task, profile: TaskProfile): Promise<SessionFacts> {
    const named = task.source === "session" ? task : this.o.contextSession;
    let saved: SavedHead | undefined;
    if (named !== undefined) {
      if (!this.o.stores.includes(named.store)) {
        throw new Error(`the configuration names no store ${named.store}`);
      }
      const heads = await sessionHeads(
        this.o.psqlCommand,
        named.store,
        [named.pathHash],
        path.join(this.o.out, SESSIONS_DIR)
      );
      saved = heads.get(named.pathHash);
    }
    const first = saved?.head.firstUser ?? null;
    const start = saved?.head.start ?? null;
    const utc = (at: number | string) => new Date(at).toISOString().slice(0, 10);
    const date: Sourced =
      first !== null
        ? { value: utc(first.timestamp), from: "the UTC date of the session's first user turn" }
        : start !== null
          ? {
              value: utc(start),
              from: "the UTC date of the session's start (its head holds no user turn)",
            }
          : {
              value: utc(Date.now()),
              from:
                saved === undefined
                  ? "neutral: the capture's own date, the task names no session"
                  : "neutral: the capture's own date, the store holds no start of the session",
            };
    return sessionFacts(saved, {
      turn: first === null ? undefined : userTurnKey(first),
      date,
      devbox: profile.profile.context === "devbox",
      devboxClaudeConfigDir: this.o.devboxClaudeConfigDir,
    });
  }

  /** Task `task`'s body through Oh My Pi in the context of `profile`, captured or read back. A
   *  capture made here lists each skill under the line the profile's recorded listing gives it
   *  (`recordedLines`), else under a hint of the test's draw. The session copy's description (a
   *  read of its repository, `sessionDescription`) is read only where `recordedLines` uses it
   *  (`readsSessionDescription`), so a read whose answer no line uses fails no capture. */
  async ompCapture(task: Task, profile: TaskProfile): Promise<Captured> {
    const files = loadCapturePaths(this.o.out, "omp", task.id);
    const { models } = this.o;
    const { c, arm } = this.caseOf(
      task.id,
      task.text,
      profile.profile,
      this.daemonImages.get(task.id)
    );
    let capture = await restoredCapture(c, arm, files, {
      source: this.o.source,
      cacheDir: this.o.cacheDir,
    });
    if (capture === undefined && models !== undefined) {
      const { name, renamedFrom } = this.settings;
      const listing = profile.profile.recordedListing;
      const tested = {
        name,
        ownName: this.settings.testSetup === undefined ? (renamedFrom ?? name) : name,
        description: skillFrontmatter(this.skillDir).description,
      };
      const lines = recordedLines(
        listing,
        tested,
        this.settings.testSetup === undefined && readsSessionDescription(listing, tested)
          ? await this.sessionDescription(profile)
          : undefined
      );
      capture = await captureBody(c, arm, {
        omp: this.o.omp,
        outDir: path.dirname(files.body),
        scratchDir: path.join(this.root, "captures"),
        cacheDir: this.o.cacheDir,
        source: this.o.source,
        models,
        hints: this.o.hints,
        ...(this.o.hintModel === undefined ? {} : { hintModel: this.o.hintModel }),
        recordedLines: lines,
        hintDraw: this.o.hintDraw,
        configFiles: this.o.configFiles,
        session: await this.sessionOf(task, profile),
      });
    }
    if (capture === undefined) {
      throw new Error(
        `it has no capture in ${path.dirname(files.body)}, and no --models is given to make one`
      );
    }
    const { name } = profile.tested;
    const listed = capture.record.listing.find((entry) => entry.name === name)?.hint;
    if (listed === undefined) throw new Error(`its capture does not list ${name}`);
    return {
      body: capture.body,
      headers: capture.record.requestHeaders,
      listed,
      contextMs: capture.made ? capture.record.timings.contextMs : 0,
      arm,
      copies: installedCopies(capture.record.context, this.o.cacheDir),
    };
  }

  /** The scratch Claude Code home of `profile` (ClaudeSetup), made once per profile: its skills
   *  and the tested one laid out by context.ts, a local marketplace of its layout's plugins
   *  installed into a scratch CLAUDE_CONFIG_DIR, and each project skill placed as Claude Code
   *  reads it (`claudeKind`): a `.claude/skills` one written at its path in the checkout, a
   *  `.omp/skills` one left out of the context. A profile's user-level context file (`user: true`,
   *  the one its dev box's host read, build.ts) is Claude Code's user memory, CLAUDE.md in its
   *  config directory: it is written there from the profile's repository at its commit, and left
   *  out of the checkout, where context.ts writes the profile's other context files. */
  claudeSetup(profile: TaskProfile): Promise<ClaudeSetup> {
    const made = this.claudeSetups.get(profile.digest);
    if (made !== undefined) return made;
    const setup = (async () => {
      const layout = profile.profile.claude;
      if (layout === undefined) throw new Error(`the profile ${profile.file} has no Claude layout`);
      const root = path.join(this.root, "claude", "profiles", profile.digest.slice(0, 16));
      rmSync(root, { recursive: true, force: true });
      const { c, arm } = this.caseOf("claude-profile", "", profile.profile);
      const [userMemory, ...more] = c.profile.contextFiles.filter((file) => file.user === true);
      if (more.length > 0) {
        throw new Error(
          `the profile ${profile.file} holds ${more.length + 1} user-level context files; Claude Code has one user memory file`
        );
      }
      const kinds = profile.profile.skills.map((skill) => ({
        skill,
        kind: claudeKind(profile.profile, skill),
      }));
      // Claude Code reads no `.omp/skills`, so such a project skill is not installed at all.
      const leftOut = new Set(
        kinds.filter(({ kind }) => kind === "left out").map(({ skill }) => skill.name)
      );
      const skills = arm.profileSkills.filter((skill) => !leftOut.has(skill.name));
      // Oh My Pi's plugins are no Claude Code plugins: only the skills and context files go in.
      const contextStarted = performance.now();
      const context = await buildContext(
        {
          ...c,
          profile: {
            ...c.profile,
            skills,
            plugins: [],
            contextFiles: c.profile.contextFiles.filter((file) => file !== userMemory),
          },
        },
        { ...arm, profileSkills: skills },
        { source: this.o.source, cacheDir: this.o.cacheDir, root: path.join(root, "context") }
      );
      const memory =
        userMemory === undefined
          ? undefined
          : await this.o.source.file(userMemory.repo, userMemory.commit, userMemory.path);
      const contextMs = Math.round(performance.now() - contextStarted);
      const plugins = layout.plugins.map((plugin) => ({ ...plugin, skills: [...plugin.skills] }));
      const own = plugins.find((plugin) => plugin.name === layout.skillPlugin);
      if (own === undefined) {
        plugins.push({ name: layout.skillPlugin, skills: [profile.tested.name] });
      } else own.skills.push(profile.tested.name);
      const placements: ClaudePlacement[] = [];
      for (const { skill, kind } of kinds) {
        if (kind === "left out") {
          placements.push({ skill: skill.name, as: kind, reason: projectSkillWords(skill, kind) });
        } else if (kind === "checkout") {
          // Its files as the context installed them, at its path in the session's checkout.
          const installed = context.skills.find((each) => each.name === skill.name);
          if (installed === undefined) {
            throw new Error(`the context installs no skill ${skill.name}`);
          }
          cpSync(installed.dir, path.join(context.checkout, skill.path), {
            recursive: true,
            verbatimSymlinks: true,
          });
          placements.push({ skill: skill.name, as: kind, path: skill.path });
        } else {
          const plugin = plugins.find((each) => each.skills.includes(skill.name));
          if (plugin === undefined) throw new Error(`the layout puts ${skill.name} in no plugin`);
          placements.push({ skill: skill.name, as: kind, plugin: plugin.name });
        }
      }
      placements.push({ skill: profile.tested.name, as: "plugin", plugin: layout.skillPlugin });
      const market = path.join(root, "marketplace");
      writeWhole(
        path.join(market, ".claude-plugin", "marketplace.json"),
        json({
          name: layout.marketplace,
          owner: { name: layout.marketplace },
          plugins: plugins.map((plugin) => ({
            name: plugin.name,
            source: `./plugins/${plugin.name}`,
          })),
        })
      );
      for (const plugin of plugins) {
        const dir = path.join(market, "plugins", plugin.name);
        writeWhole(
          path.join(dir, ".claude-plugin", "plugin.json"),
          json({
            name: plugin.name,
            version: "0.0.0",
            description: `The ${plugin.name} plugin of a skill-cuts load test's profile.`,
          })
        );
        for (const name of plugin.skills) {
          const installed = context.skills.find((skill) => skill.name === name);
          if (installed === undefined) throw new Error(`the context installs no skill ${name}`);
          cpSync(installed.dir, path.join(dir, "skills", name), {
            recursive: true,
            verbatimSymlinks: true,
          });
        }
      }
      const home = path.join(root, "home");
      const tmp = path.join(root, "tmp");
      const config = path.join(root, "config");
      for (const dir of [home, tmp, config]) mkdirSync(dir, { recursive: true });
      if (memory !== undefined) writeFileSync(path.join(config, "CLAUDE.md"), memory);
      const env: Record<string, string> = {
        HOME: home,
        PATH: "/usr/local/bin:/usr/bin:/bin",
        TMPDIR: tmp,
        TZ: "UTC",
        CLAUDE_CONFIG_DIR: config,
        DISABLE_AUTOUPDATER: "1",
        DISABLE_TELEMETRY: "1",
        DISABLE_ERROR_REPORTING: "1",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
      };
      const version = (
        await mustRun([this.o.claude, "--version"], env, CLAUDE_VERSION_TIMEOUT_MS)
      ).trim();
      await mustRun(
        [this.o.claude, "plugin", "marketplace", "add", market],
        env,
        CLAUDE_PLUGIN_TIMEOUT_MS
      );
      for (const plugin of plugins) {
        await mustRun(
          [this.o.claude, "plugin", "install", `${plugin.name}@${layout.marketplace}`],
          env,
          CLAUDE_PLUGIN_TIMEOUT_MS
        );
      }
      return {
        env,
        workspace: context.workspace,
        checkout:
          c.profile.workingDirectory === undefined
            ? undefined
            : { dir: context.checkout, workingDirectory: c.profile.workingDirectory },
        prompt:
          context.appendSystemPrompt === undefined
            ? undefined
            : readFileSync(context.appendSystemPrompt, "utf8"),
        version,
        context: context.record,
        userMemory: userMemory ?? null,
        placements,
        contextMs,
      };
    })();
    this.claudeSetups.set(profile.digest, setup);
    return setup;
  }

  /** Task `task`'s body through Claude Code in the context of `profile`, captured or read back;
   *  a saved one whose record holds no harness-string record was made before that rule, and is
   *  refused. Before it is saved, the body's strings of the capture's run are replaced by the
   *  session's (`claudeHarnessRules`), and a body that still holds the test's scratch is
   *  refused. */
  async claudeCapture(task: Task, profile: TaskProfile): Promise<Captured> {
    const files = loadCapturePaths(this.o.out, "claude", task.id);
    // Its runs end at their first message (see the file's header): they answer no read, so they
    // need no installed copies, and the arm is there for replay.ts's `replayOnce` alone.
    const { arm } = this.caseOf(task.id, task.text, profile.profile);
    if (existsSync(files.body) && existsSync(files.record)) {
      const record = JSON.parse(readFileSync(files.record, "utf8")) as ClaudeCaptureRecord;
      if (!Object.hasOwn(record, "harness")) {
        throw new Error(
          `the capture ${files.body} was made before the harness-string rule, so its body holds the capture's own paths and date: make it again under a new --out`
        );
      }
      return {
        body: JSON.parse(readFileSync(files.body, "utf8")) as MessagesBody,
        headers: record.requestHeaders,
        listed: record.listed,
        contextMs: 0,
        arm,
        copies: [],
      };
    }
    const facts = await this.sessionOf(task, profile);
    // The context is built by the first task of its profile, which counts its time.
    const setUpNow = !this.claudeSetups.has(profile.digest);
    const setup = await this.claudeSetup(profile);
    const { thinking } = this.settings;
    const model = this.settings.model.replace(/^anthropic\//, "");
    const logs = path.join(this.root, "claude", "runs", cacheKey(task.id).slice(0, 16));
    mkdirSync(logs, { recursive: true });
    const stderrFile = path.join(logs, "stderr.txt");
    const recorder = startRecorder();
    const started = performance.now();
    let requests: RecordedRequest[];
    try {
      const child = Bun.spawn(
        [
          this.o.claude,
          "-p",
          "--output-format",
          "stream-json",
          "--verbose",
          "--model",
          model,
          ...(CLAUDE_EFFORTS.includes(thinking) ? ["--effort", thinking] : []),
          ...(setup.prompt === undefined ? [] : ["--append-system-prompt", setup.prompt]),
          // After `--`, so a task's text that starts with `-` is read as the prompt.
          "--",
          task.text,
        ],
        {
          cwd: setup.workspace,
          env: {
            ...setup.env,
            ANTHROPIC_BASE_URL: recorder.url,
            ANTHROPIC_API_KEY: RECORDER_CREDENTIAL,
            // A request for a smaller model asks for the tested one; the recorder answers each.
            ANTHROPIC_DEFAULT_HAIKU_MODEL: model,
            ANTHROPIC_SMALL_FAST_MODEL: model,
            ...(thinking === "off" ? { CLAUDE_CODE_DISABLE_THINKING: "1" } : {}),
          },
          stdin: "ignore",
          stdout: Bun.file(path.join(logs, "stream.jsonl")),
          stderr: Bun.file(stderrFile),
        }
      );
      const timer = setTimeout(() => child.kill("SIGKILL"), RUN_TIMEOUT_MS);
      const code = await child.exited;
      clearTimeout(timer);
      if (code !== 0) {
        const tail = readFileSync(stderrFile, "utf8").trim().split("\n").slice(-12).join("\n");
        throw new Error(`Claude Code ended with ${child.signalCode ?? `exit ${code}`}: ${tail}`);
      }
      requests = [...recorder.requests];
    } finally {
      await recorder.close();
    }
    const runMs = Math.round(performance.now() - started);
    const turn = requests.find((request) => {
      if (request.method !== "POST" || request.path.split("?")[0] !== "/v1/messages") return false;
      const body: unknown = JSON.parse(request.body);
      const tools = typeof body === "object" && body !== null ? Reflect.get(body, "tools") : null;
      return Array.isArray(tools) && bodyTexts(body).some((text) => text.includes(task.text));
    });
    if (turn === undefined) throw new Error("Claude Code sent no model request that holds it");
    const raw = JSON.parse(turn.body) as MessagesBody;
    const configDir = setup.env.CLAUDE_CONFIG_DIR ?? "";
    const rewritten = rewriteBody(
      raw,
      claudeHarnessRules(setup.workspace, configDir, facts, setup.checkout),
      [this.root]
    );
    if (rewritten.faults.length > 0) {
      throw new Error(
        `its captured body is refused: it still holds the test's scratch after the harness-string rule, at ${rewritten.faults.join("; ")}`
      );
    }
    const body = rewritten.body as MessagesBody;
    const listing = claudeListing(body);
    const key = `${profile.tested.plugin}:${profile.tested.name}`;
    const listed = listing.get(key);
    if (listed === undefined) {
      const names = [...listing.keys()].join(", ") || "no skill";
      throw new Error(`its captured body is refused: Claude Code lists ${names}, not ${key}`);
    }
    const record: ClaudeCaptureRecord = {
      task: task.id,
      capturedAt: new Date().toISOString(),
      claude: { binary: this.o.claude, version: setup.version },
      model,
      thinking,
      skill: profile.tested,
      profile: profile.file,
      profileDigest: profile.digest,
      requestPath: turn.path,
      requestHeaders: anthropicHeaders(turn.headers),
      listed,
      listedSource: CLAUDE_LISTED,
      listing: [...listing].map(([name, description]) => ({ name, description })),
      otherRequests: requests
        .filter((request) => request !== turn)
        .map((request) => ({ path: request.path, bytes: request.body.length })),
      context: setup.context,
      userMemory: setup.userMemory,
      sessionRepository: profile.profile.sessionRepository ?? { none: NO_SESSION_REPOSITORY },
      placements: setup.placements,
      runMs,
      harness: {
        ...facts,
        os: { value: CLAUDE_NEUTRAL_OS, from: "neutral: the store records no OS of the session" },
        replaced: rewritten.replaced,
        rawBody: rawBodyFile(files.body),
      },
    };
    writeWhole(rawBodyFile(files.body), turn.body);
    writeWhole(files.body, JSON.stringify(body));
    writeWhole(files.record, json(record));
    return {
      body,
      headers: record.requestHeaders,
      listed,
      contextMs: setUpNow ? setup.contextMs : 0,
      arm,
      copies: [],
    };
  }

  /** The runs task `id` has saved. Throws a refusal when its file holds a line that is not
   *  its next run, or a run made under a profile of other bytes than `profile`. */
  savedRuns(id: string, profile: TaskProfile): LoadRun[] {
    const file = loadRunFile(this.o.out, id);
    const runs: LoadRun[] = [];
    for (const { at, value } of jsonLines(file)) {
      const parsed = LoadRun.safeParse(value);
      if (!parsed.success || parsed.data.task !== id || parsed.data.run !== runs.length + 1) {
        throw new Refused(`${file}:${at} is not run ${runs.length + 1} of task ${id}`);
      }
      const run = parsed.data;
      if (run.profileDigest !== profile.digest) {
        throw new Refused(
          `${file}:${at}: run ${run.run} of task ${id} ran under the profile ${run.profile} (SHA-256 ${run.profileDigest}), not ${profile.file} (SHA-256 ${profile.digest}): give the task that profile again, or use another --out`
        );
      }
      runs.push(run);
    }
    return runs;
  }

  /** The failed runs task `id` has saved (FailedRun). Throws a refusal when its file holds a line
   *  that is not one of task `id`. */
  savedFailedRuns(id: string): FailedRun[] {
    const file = loadFailedRunFile(this.o.out, id);
    const failed: FailedRun[] = [];
    for (const { at, value } of jsonLines(file)) {
      const parsed = FailedRun.safeParse(value);
      if (!parsed.success || parsed.data.task !== id) {
        throw new Refused(`${file}:${at} is not a failed run of task ${id}`);
      }
      failed.push(parsed.data);
    }
    return failed;
  }

  /**
   * One run of `each`'s capture (see the file's header), sent through replay.ts's `replayOnce`
   * with the routing id `routingId` (none beside a Responses body): it ends at a reply that loads
   * the skill, once the answer to a read of another skill names the skill, on Claude Code at its
   * first reply, else as the replay ends a run, which on Oh My Pi sorts a reply that reads skill
   * files beside other calls by LOAD_RULE. Gives what it came to and each of its replies.
   * `firstSettled` is told once its first send has its reply or its failure. Rejects with
   * RunFailed when a send does (the gateway's `BudgetSpent` among them), which carries what the
   * run's earlier replies used.
   */
  async loadRun(each: ReadyTask, firstSettled: () => void, routingId: string): Promise<RunDone> {
    const { host } = this.o;
    const { tested } = each.profile;
    // What each reply used, in the order the replies came, kept as each one comes so that a run
    // that fails later still counts it.
    const spent: Spent[] = [];
    const gateway: Gateway = {
      send: async (body, options) => {
        const reply = await this.o.gateway.send(body, options);
        spent.push(reply.spent);
        return reply;
      },
      spend: () => this.o.gateway.spend(),
    };
    const seen: Omit<ReplyRecord, "usage">[] = [];
    let ended: { readonly outcome: "loaded" | "sent by"; readonly by: string } | undefined;
    const endsAt = (
      content: readonly ContentBlock[],
      round: readonly SkillRead[] | undefined
    ): boolean => {
      const load = messageLoad(host, content, tested);
      seen.push({
        calls: toolCalls(content).map((call) => {
          const argument = ruleArgument(host, call);
          return {
            name: call.name ?? "",
            target: callTarget(host, call) ?? null,
            ...(argument === undefined ? {} : { argument }),
          };
        }),
        // AnsweredRead's fields, as ReplyRecord keeps them.
        answers:
          round?.map(({ read: { skill, file, by, from, isError } }) => ({
            skill,
            file,
            by,
            from,
            isError,
          })) ?? null,
      });
      if (load.loadedBy !== null) {
        ended = { outcome: "loaded", by: load.loadedBy };
        return true;
      }
      if (host === "claude") return true;
      const sender = round === undefined ? undefined : senderOf(round, tested.name);
      if (sender === undefined) return false;
      ended = { outcome: "sent by", by: sender.read.skill };
      return true;
    };
    const { body, headers, arm, copies } = each.captured;
    let replayed: Replayed;
    let unjudged:
      | { readonly outcome: "reads past last round" | "undecided"; readonly reason: string }
      | undefined;
    try {
      replayed = await replayOnce(
        body,
        arm,
        gateway,
        // Claude Code's runs end at their first message, so they take no rule.
        { headers, copies, endsAt, routingId, ...(host === "omp" ? { mixed: LOAD_RULE } : {}) },
        firstSettled
      );
    } catch (error) {
      if (error instanceof ReadsPastLastRound) {
        unjudged = { outcome: "reads past last round", reason: error.message };
      } else if (error instanceof UndecidedReply) {
        unjudged = { outcome: "undecided", reason: error.message };
      } else throw new RunFailed(error, spent);
      replayed = error.replayed;
    }
    const replies = seen.map((reply, index) => {
      const usage = spent[index];
      if (usage === undefined) throw new Error(`the gateway gave no spend of reply ${index + 1}`);
      return { ...reply, usage: { ...usage } };
    });
    return {
      outcome: ended?.outcome ?? unjudged?.outcome ?? "not loaded",
      loadedBy: ended?.outcome === "loaded" ? ended.by : null,
      sentBy: ended?.outcome === "sent by" ? ended.by : null,
      skillReads: host === "omp" ? "answered" : "first message",
      replies,
      ...(replayed.leftOut.length === 0 ? {} : { leftOut: [...replayed.leftOut] }),
      ...(unjudged?.outcome === "undecided" ? { reason: unjudged.reason } : {}),
      stopReason: replayed.step.message.stop_reason,
      requestId: replayed.step.requestId ?? null,
      userId: replayed.userId,
      routingId: replayed.routingId,
      deployments: replayed.deployments,
    };
  }

  /** Task `task`'s capture in the context of `profile` (made, or read back), ready for its runs,
   *  with the time it took and the GitHub reads made in it. */
  async captureTask(task: Task, profile: TaskProfile): Promise<ReadyTask> {
    const readsBefore = this.o.source.reads?.() ?? { calls: 0, ms: 0 };
    const started = performance.now();
    const captured =
      this.o.host === "omp"
        ? await this.ompCapture(task, profile)
        : await this.claudeCapture(task, profile);
    const readsAfter = this.o.source.reads?.() ?? { calls: 0, ms: 0 };
    return {
      task,
      profile,
      captured,
      captureMs: performance.now() - started,
      github: { calls: readsAfter.calls - readsBefore.calls, ms: readsAfter.ms - readsBefore.ms },
    };
  }

  /**
   * `count` runs of each task of `batch` (one profile's tasks), each after the runs it `made`,
   * sent in one order, replay.ts's `sendOrder` under a fresh seed, never one task's batch after
   * another's; each run is `loadRun`'s, with the routing id of the number it takes if no run of
   * its task before it fails (`loadRoutingId`: the task's runs so far, then its runs in the order
   * sent). The runs start in that order, all at once but for one wait: the first run of each
   * task in the order goes alone among its task's, the next run in the order starting only once
   * its first send has its reply or has failed, so that the prefix it wrote to the cache serves
   * its task's later runs; the waits change no place of the order. Gives each task's runs, those
   * it made first and the new ones in the order sent; its failed runs, those it had (`failed`)
   * first and then each new run that failed after one or more of its requests had their replies,
   * the budget's refusal among them; each run that failed, whether the budget refused one, the
   * seed and the time.
   */
  async sendRuns(
    batch: readonly {
      readonly each: ReadyTask;
      readonly made: readonly LoadRun[];
      readonly failed: readonly FailedRun[];
      readonly count: number;
    }[]
  ): Promise<{
    readonly runs: ReadonlyMap<string, LoadRun[]>;
    readonly failed: ReadonlyMap<string, FailedRun[]>;
    readonly failures: string[];
    readonly budgetSpent: boolean;
    readonly seed: string;
    readonly runsMs: number;
  }> {
    const seed = newSeed();
    const byId = new Map(batch.map((entry) => [entry.each.task.id, entry.each]));
    const order = sendOrder(
      batch.map(({ each, count }) => ({ key: each.task.id, runs: count })),
      seed
    );
    // How many runs of each task have started, in the send order.
    const started = new Map<string, number>();
    const madeOf = new Map(batch.map(({ each, made }) => [each.task.id, made.length]));
    const sends: { id: string; place: number; run: Promise<RunDone> }[] = [];
    const runsStarted = performance.now();
    for (const [place, { key }] of order.entries()) {
      const each = byId.get(key);
      if (each === undefined) throw new Error(`no task ${key}`);
      const nth = started.get(key) ?? 0;
      started.set(key, nth + 1);
      const number = (madeOf.get(key) ?? 0) + nth + 1;
      const first = Promise.withResolvers<void>();
      const run = this.loadRun(
        each,
        first.resolve,
        loadRoutingId(this.o.routeSeed ?? "", key, number)
      );
      // Its outcome is read once every run has started; until then a failure is held, not thrown.
      const settledRun = run.then(
        () => undefined,
        () => undefined
      );
      sends.push({ id: key, place, run });
      // A run that fails before its first send has settled too.
      if (nth === 0) await Promise.race([first.promise, settledRun]);
    }
    const settled = await Promise.allSettled(sends.map((send) => send.run));
    const runsMs = performance.now() - runsStarted;
    const runs = new Map(batch.map(({ each, made }) => [each.task.id, [...made]]));
    const failed = new Map(batch.map((entry) => [entry.each.task.id, [...entry.failed]]));
    const failures: string[] = [];
    let budgetSpent = false;
    for (const [index, { id, place }] of sends.entries()) {
      const outcome = settled[index];
      const each = byId.get(id);
      const made = runs.get(id);
      if (outcome === undefined || each === undefined || made === undefined) continue;
      if (outcome.status === "fulfilled") {
        // RunDone's fields go where LoadRun has them (`loadRun` gives them in that order), its
        // session id, routing id and deployments after the run's listing and profile.
        const { userId, routingId, deployments, ...replied } = outcome.value;
        made.push({
          task: each.task.id,
          run: made.length + 1,
          label: each.task.label,
          ...(this.settings.testSetup === undefined ? {} : { testSetup: this.settings.testSetup }),
          source: each.task.source,
          host: this.o.host,
          name: each.profile.tested.name,
          ...replied,
          listed: each.captured.listed,
          profile: each.profile.file,
          profileDigest: each.profile.digest,
          userId,
          routingId,
          deployments,
          sendSeed: seed,
          sendPlace: place,
        });
      } else {
        const { reason } = outcome;
        const cause = reason instanceof RunFailed ? reason.cause : reason;
        const usage = reason instanceof RunFailed ? reason.usage : [];
        if (usage.length > 0) {
          failed.get(id)?.push({
            task: id,
            ...(this.settings.testSetup === undefined
              ? {}
              : { testSetup: this.settings.testSetup }),
            error: messageFor(cause),
            usage: [...usage],
          });
        }
        if (cause instanceof BudgetSpent) budgetSpent = true;
        else failures.push(`task ${id}: a run failed: ${messageFor(cause)}`);
      }
    }
    return { runs, failed, failures, budgetSpent, seed, runsMs };
  }
}

/** The seconds of the phases of a task that took `totalMs` in all (its own capture step, its
 *  group's runs and its writes), as its line ends them: its capture (the context build and the
 *  GitHub reads within it apart), its runs, its writes, and the rest. */
function phaseWords(phases: TaskPhases, totalMs: number): string {
  const s = (ms: number) => (ms / 1000).toFixed(1);
  const other = Math.max(0, totalMs - phases.captureMs - phases.runsMs - phases.writeMs);
  return `seconds: capture ${s(phases.captureMs)} (context ${s(phases.contextMs)}; ${phases.github.calls} GitHub reads, ${s(phases.github.ms)}), runs ${s(phases.runsMs)}, write ${s(phases.writeMs)}, other ${s(other)}, total ${s(totalMs)}`;
}

/** Task `task`'s verdict in words. */
function verdictWords(task: Task, outcome: TaskOutcome, wanted: number): string {
  if (outcome.noEvidence !== null) return `no description evidence: ${outcome.noEvidence}`;
  if (!outcome.complete) {
    return `${outcome.judged} of ${wanted} judged runs made; run the command again for the rest`;
  }
  if (outcome.fault === null)
    return `no verdict: the load test's rule takes ${LOAD_RUNS} runs a task`;
  if (!outcome.fault) return "no fault";
  return task.label === "need"
    ? `a fault: a need task loads it in fewer than ${NEED_MIN_LOADS} of ${LOAD_RUNS} runs`
    : `a fault: a near-miss task loads it in more than ${NEAR_MISS_MAX_LOADS} of ${LOAD_RUNS} runs`;
}

/** A task's counts in words (TaskOutcome): its loads among its judged runs, and what each of
 *  `runs` came to when they are given; its runs each other skill sent, its runs that read past
 *  the last round, its runs undecided at a mixed reply where it has any, and its extra runs. */
function countWords(outcome: TaskOutcome, runs?: readonly LoadRun[]): string {
  const senders = Object.entries(outcome.sentBy);
  const sent = senders.reduce((sum, [, count]) => sum + count, 0);
  const bySkill =
    senders.length === 0
      ? ""
      : ` (${senders.map(([skill, count]) => `${skill} ${count}`).join(", ")})`;
  const each = runs === undefined ? "" : ` (${runs.map(runWords).join(", ")})`;
  const undecided =
    outcome.undecided === 0 ? "" : `; runs undecided at a mixed reply: ${outcome.undecided}`;
  return `loaded in ${outcome.loads} of ${outcome.judged} judged runs${each}; runs sent by another skill: ${sent}${bySkill}; runs that read past the last round: ${outcome.readsPastLastRound}${undecided}; extra runs: ${outcome.extraRuns}`;
}

/** What run `run` came to, in words. */
function runWords(run: LoadRun): string {
  switch (run.outcome) {
    case "loaded":
      return `run ${run.run} loads`;
    case "not loaded":
      return `run ${run.run} does not`;
    case "sent by":
      return `run ${run.run} is sent by ${run.sentBy}`;
    case "reads past last round":
      return `run ${run.run} reads past the last round`;
    case "undecided":
      return `run ${run.run} is undecided`;
  }
}

/** Runs the load test `o` names (see the file's header). Throws a refusal before any model call
 *  when an input is refused, and a Stopped, after saving, when the budget is spent or the
 *  peak memory passes `o.memoryLimitBytes`. */
export async function runLoad(o: LoadOptions): Promise<LoadResult> {
  const given = path.resolve(o.skillDir);
  if (!statSync(path.join(given, "SKILL.md"), { throwIfNoEntry: false })?.isFile()) {
    throw new Refused(`--skill ${o.skillDir} holds no SKILL.md`);
  }
  const own = skillName(given);
  const name = o.name ?? own;
  if (!NAME.test(name)) {
    throw new Refused(`${JSON.stringify(name)} is not a name a skill can be listed under`);
  }
  if (skillFrontmatter(given).hidden) {
    throw new Refused(
      `${o.skillDir} sets hide or disable-model-invocation, so no host lists it for its agent to load`
    );
  }
  if (!Number.isInteger(o.runs) || o.runs < 1) {
    throw new Refused(`--runs ${o.runs} is not a whole number of 1 or more`);
  }
  if (!Number.isInteger(o.hintDraw) || o.hintDraw < 1) {
    throw new Refused(`--hint-draw ${o.hintDraw} is not a whole number of 1 or more`);
  }
  if (o.host === "claude" && o.hintDraw > 1) {
    throw new Refused(
      `--hint-draw ${o.hintDraw} is refused on Claude Code: it lists each skill under its description, not under a hint a model writes, so it has no hint to draw again`
    );
  }
  const slash = o.model.indexOf("/");
  const provider = slash < 0 ? "anthropic" : o.model.slice(0, slash);
  const modelId = o.model.slice(slash + 1);
  if (provider !== "anthropic" && provider !== "openai") {
    throw new Refused(
      `--model ${o.model}: the runs go to the gateway's Anthropic Messages route (anthropic/<id>) or its OpenAI Responses route (openai/<id>)`
    );
  }
  if (provider === "openai" && o.host === "claude") {
    throw new Refused(
      `--model ${o.model}: Claude Code calls the Anthropic Messages API only, so its model is anthropic/<id>`
    );
  }
  if (PRICES[modelId] === undefined) {
    throw new Refused(
      `gateway.ts has no list price for ${modelId}, so the runs' dollars could not be counted`
    );
  }
  if (o.host === "claude" && o.thinking === "minimal") {
    throw new Refused(
      `Claude Code has no thinking level minimal: it takes off, auto, or ${CLAUDE_EFFORTS.join(", ")} (--effort)`
    );
  }
  const tasks = loadTasks(o.tasksFile);
  const marked = tasks.flatMap((task) =>
    task.testSetup === undefined ? [] : [{ id: task.id, copy: task.testSetup.copy }]
  );
  if (o.testSetup === undefined && marked.length > 0) {
    throw new Refused(
      `${o.tasksFile}: tasks ${marked.map((task) => task.id).join(", ")} record "${TEST_SETUP_LABEL}"; give the matching --test-setup`
    );
  }
  const at = o.testSetup;
  let testSetup: TestSetup | undefined;
  let setupDir: string | undefined;
  // Remove the temporary inventory checkout on every exit.
  using inventory = {
    root: undefined as string | undefined,
    [Symbol.dispose]() {
      if (this.root !== undefined) rmSync(this.root, { recursive: true, force: true });
    },
  };
  if (at !== undefined) {
    for (const task of marked) {
      if (!isSetupCopy(task.copy, at)) {
        throw new Refused(
          `task ${task.id} records "${TEST_SETUP_LABEL}" with ${setupLocation(task.copy)}, not --test-setup ${setupLocation(at)}`
        );
      }
    }
    // Read the complete directory even when the task file already staged the profile entry.
    testSetup = await resolveTestSetup(at, own, async () => {
      mkdirSync(o.scratch, { recursive: true });
      inventory.root = mkdtempSync(path.join(o.scratch, "inventory-"));
      setupDir = path.join(inventory.root, "skill");
      await setupDirectory(at, o.source, setupDir);
      return ompText(readFileSync(path.join(setupDir, "SKILL.md")));
    });
  }
  const setupNote = testSetup === undefined ? {} : { testSetup };
  const profiled = taskProfiles(tasks, o, name, testSetup);
  const injected = await injectedRefusal(profiled, o, own, name);
  if (injected !== undefined) throw new Refused(injected);
  // Each task's standing by its listing measure, and the tasks the listing rule drops: never run.
  const evidence = summaryEvidence(o.tasksFile, tasks);
  const { standings: listings, fromRecorded } = taskListings(o.tasksFile, tasks, evidence, o.host);
  // Each task's daemon image item (context.ts's DAEMON_IMAGE_ITEM), which its capture reads, so
  // a Legion task's role prompt is composed at its daemon image's commit, as a behaviour case's
  // is. No other item of the evidence goes to the capture.
  const daemonImages = new Map(
    tasks.map((task) => [
      task.id,
      (evidence?.get(task.id) ?? []).filter((each) => each.item.startsWith(DAEMON_IMAGE_ITEM)),
    ])
  );
  const listingDrops = new Map(
    [...listings].flatMap(([id, standing]) =>
      standing.state === "dropped" ? [[id, standing.reason] as const] : []
    )
  );
  const standings = [...listings.values()];
  const listingCounts = {
    summary: `${o.tasksFile}.summary.json`,
    measured: standings.filter((standing) => standing.state === "measured").length,
    notMeasurable: standings.filter((standing) => standing.state === "not measurable").length,
    dropped: listingDrops.size,
    // The drops by the start of their reason: a task whose listing both misses and adds too many
    // names is counted under listing-misses (score.ts's `listingStanding`).
    droppedBy: Object.fromEntries(
      [LISTING_MISSES, LISTING_ADDS].map((start) => [
        start,
        [...listingDrops.values()].filter((reason) => reason.startsWith(start)).length,
      ])
    ),
    // The tasks whose profile took names from their recorded listing (build.ts's fromRecorded):
    // their measure cannot see a miss of those names.
    fromRecorded,
  };
  // Each task whose capture carries the Envoy plugin's block of the skill, by where the answer
  // that its session had Dispatch comes from (arms.ts's `dispatchOf`), counted beside the listing.
  // A load test has no transcript messages, so the answer comes from a Legion role or is assumed.
  const froms =
    o.host === "omp"
      ? profiled.flatMap(({ profile }) =>
          pluginInjects({ skill: own, profile: profile.profile })
            ? [dispatchOf({ context: profile.profile.context, messages: [] }).from]
            : []
        )
      : [];
  const dispatchCounts = {
    carryBlock: froms.length,
    assumed: froms.filter((from) => from === "assumed").length,
    legionRole: froms.filter((from) => from === "legion-role").length,
  };
  // With no --hint-model the hint model is the model itself, and Oh My Pi's hints are written on
  // the Messages route only, so each capture to be made would fail.
  if (
    o.host === "omp" &&
    provider === "openai" &&
    o.hintModel === undefined &&
    !tasks.some((task) => {
      const files = loadCapturePaths(o.out, "omp", task.id);
      return existsSync(files.body) && existsSync(files.record);
    })
  ) {
    throw new Refused(
      `--model ${o.model} is given with no --hint-model, so the hint model is ${o.model}, and Oh My Pi's hints are written on the Anthropic Messages route only: each task's capture would fail. Give --hint-model anthropic/<id>`
    );
  }
  const settings: Settings = {
    skillText: o.skillDigest,
    name,
    renamedFrom: name === own ? null : own,
    host: o.host,
    model: `${provider}/${modelId}`,
    thinking: o.thinking,
    runs: o.runs,
    hintModel: o.host === "omp" ? (o.hintModel ?? null) : null,
    // Claude Code lists descriptions: its settings name no hints, as before this rule.
    ...(o.host === "omp" ? { hints: { listed: LISTED_LINES, draw: o.hintDraw } } : {}),
    sends: SENDS,
    skillReads: SKILL_READS,
    ...setupNote,
  };

  const summaryFile = path.join(o.out, "summary.json");
  // Each task dropped, by its id, with why: by the listing rule, before any run; or for a
  // directory of its context no read can give, which an earlier command dropped under this
  // profile and which is not tried again.
  const dropped = new Map<string, string>(listingDrops);
  if (
    testSetup !== undefined &&
    !existsSync(summaryFile) &&
    tasks.some(
      (task) =>
        existsSync(loadCapturePaths(o.out, o.host, task.id).body) ||
        existsSync(loadRunFile(o.out, task.id)) ||
        existsSync(loadFailedRunFile(o.out, task.id))
    )
  ) {
    throw new Refused(
      `${o.out} holds captures or runs but no summary to confirm "${TEST_SETUP_LABEL}"; use another --out`
    );
  }
  if (existsSync(summaryFile)) {
    let raw: unknown;
    try {
      raw = JSON.parse(readFileSync(summaryFile, "utf8"));
    } catch {
      raw = undefined;
    }
    const saved = SavedSummary.safeParse(raw);
    if (!saved.success) throw new Refused(`${summaryFile} is not a load test's summary`);
    if (JSON.stringify(saved.data.settings) !== JSON.stringify(settings)) {
      throw new Refused(
        `${o.out} holds a load test of other settings (${JSON.stringify(saved.data.settings)}) than this one's (${JSON.stringify(settings)}): use another --out`
      );
    }
    for (const { task, profile } of profiled) {
      const before = saved.data.tasks.find((each) => each.id === task.id);
      if (before !== undefined && before.text !== task.text) {
        throw new Refused(
          `${o.out} holds runs of a task ${task.id} with another text: give the new text another id, or use another --out`
        );
      }
      // A capture or a run made under a profile of other bytes would not be this task's.
      const made =
        existsSync(loadCapturePaths(o.out, o.host, task.id).body) ||
        existsSync(loadRunFile(o.out, task.id));
      if (made && before === undefined) {
        throw new Refused(
          `${o.out} holds a capture or runs of task ${task.id}, which its summary does not list, so the text and the profile they were made for cannot be checked: give the task another id, or use another --out`
        );
      }
      if (made && before !== undefined && before.profileDigest !== profile.digest) {
        throw new Refused(
          `${o.out} holds task ${task.id} made under a profile of other bytes (SHA-256 ${before.profileDigest ?? "not recorded"}) than ${profile.file} (SHA-256 ${profile.digest}): give the task that profile again, or use another --out`
        );
      }
      // A listing drop is the listing rule's again on this run's summary, never a saved one.
      if (
        typeof before?.dropped === "string" &&
        before.dropped.startsWith(UNREADABLE_DIRECTORY) &&
        before.profileDigest === profile.digest &&
        !dropped.has(task.id)
      ) {
        dropped.set(task.id, before.dropped);
      }
    }
  }

  // A Legion task's capture composed its role prompt at the commit its daemon image item named
  // when it was made (context.ts); read back for evidence that names another, it would not be
  // this task's capture.
  for (const { task } of profiled) {
    const fault = rolePromptFault(
      loadCapturePaths(o.out, o.host, task.id).record,
      daemonImages.get(task.id) ?? []
    );
    if (fault !== undefined) {
      throw new Refused(
        `${o.out} holds task ${task.id}, whose ${fault}: give the tasks file that summary again, or use another --out`
      );
    }
  }

  const root = path.join(o.scratch, `load-${cacheKey(JSON.stringify(settings)).slice(0, 16)}`);
  const test = new LoadTest(
    o,
    settings,
    root,
    settings.renamedFrom === null
      ? (setupDir ?? given)
      : renamedSkill(setupDir ?? given, name, path.join(root, "skill")),
    daemonImages
  );
  const runsOf = new Map(
    profiled.map(({ task, profile }) => [task.id, test.savedRuns(task.id, profile)])
  );
  const failedOf = new Map(tasks.map((task) => [task.id, test.savedFailedRuns(task.id)]));
  /** A task's outcome from the runs it has so far. */
  const outcomeOf = (task: Task): TaskOutcome =>
    taskOutcome(task.label, runsOf.get(task.id) ?? [], o.runs);
  // A task whose run file holds runs makes its further runs with the saved capture those runs were
  // made with: one whose capture is gone, or lists the tested skill under another line, is refused
  // before any capture or run (`heldCaptureFault`), so no capture is made over saved runs. A task
  // that is dropped, or whose run file is complete, takes no capture.
  const heldFaults = profiled.flatMap(({ task, profile }) => {
    const runs = runsOf.get(task.id) ?? [];
    if (runs.length === 0 || dropped.has(task.id) || outcomeOf(task).toStart === 0) return [];
    const fault = heldCaptureFault(o.out, o.host, task.id, profile.tested.name, runs);
    return fault === undefined ? [] : [fault];
  });
  if (heldFaults.length > 0) {
    throw new Refused(
      `${heldFaults.join("; ")}: a task whose run file holds runs makes its further runs with the saved capture those runs were made with, and makes no capture over them, so that no task's runs mix two requests; a run records of its capture the line it lists the tested skill under, which that capture must list it under. Put each capture back as it was saved, or use another --out`
    );
  }
  let peak = 0;
  // Each task's hint dollars, and the line its capture lists the tested skill under with where
  // that line came from, from its capture's record: read at the start, and again after the task's
  // capture step, since its capture may have been made then.
  const hintDollars = new Map<string, number>();
  const listedUnder = new Map<string, ListedUnder>();
  const readCapture = (task: Task): void => {
    const record = loadCapturePaths(o.out, o.host, task.id).record;
    if (!existsSync(record)) return;
    const raw: unknown = JSON.parse(readFileSync(record, "utf8"));
    const saved = SavedListed.safeParse(raw).data;
    if (o.host === "claude") {
      if (saved?.listed !== undefined) {
        listedUnder.set(task.id, { source: CLAUDE_LISTED, line: saved.listed });
      }
      return;
    }
    hintDollars.set(task.id, hintUsd(raw as CaptureRecord));
    const entry = saved?.hints?.find((each) => each.skill === name);
    if (entry !== undefined) {
      listedUnder.set(task.id, { source: entry.source ?? NO_SOURCE, line: entry.hint });
    }
  };
  for (const task of tasks) readCapture(task);
  /** How many tasks list the tested skill under a recorded line, a drawn hint, and Claude Code's
   *  description, and how many have no capture whose record says (`none`). */
  const listedCounts = () => {
    const sources = tasks.map((task) => listedUnder.get(task.id)?.source ?? "");
    const recorded = sources.filter((source) => source.startsWith("recorded (")).length;
    const drawn = sources.filter((source) => source.startsWith("drawn (")).length;
    const description = sources.filter((source) => source === CLAUDE_LISTED).length;
    return { recorded, drawn, description, none: tasks.length - recorded - drawn - description };
  };
  /** Task `task`'s dollars so far at list price: its runs', its failed runs' (how many, and what
   *  their replies used) and its capture's hints'. */
  const spendOf = (task: Task): TaskSpend => {
    const runsUsd = (runsOf.get(task.id) ?? []).reduce(
      (sum, run) => sum + usdOf(run.replies.map((reply) => reply.usage)),
      0
    );
    const failed = failedOf.get(task.id) ?? [];
    const failedRunsUsd = failed.reduce((sum, run) => sum + usdOf(run.usage), 0);
    const hintsUsd = hintDollars.get(task.id) ?? 0;
    return {
      runsUsd,
      failedRuns: failed.length,
      failedRunsUsd,
      hintsUsd,
      usd: runsUsd + failedRunsUsd + hintsUsd,
    };
  };
  // Each task whose capture sends its session's recorded system prompt whole (an Oh My Pi task
  // whose profile holds one; Claude Code's capture sends none), with the names that prompt lists
  // as the capture sends it: a test under another name renames the tested skill's line in it.
  const recordedListings = profiled.flatMap(({ task, profile }) => {
    const prompt = profile.profile.systemPrompt;
    if (o.host !== "omp" || prompt === undefined) return [];
    const sent =
      settings.renamedFrom === null
        ? prompt
        : renamedListingBlocks(prompt, settings.renamedFrom, name);
    return [{ id: task.id, recorded: new Set(listingOf(sent).keys()) }];
  });
  /** Writes the summary as the runs stand; returns the skill's verdict, the test's dollars so far
   *  at list price (its runs', its failed runs' and its hints'), the runs of its recorded-prompt
   *  tasks that read a skill outside the recorded listing (`outsideListing`), and its runs counted
   *  by the deployment that answered them, in words (gateway.ts's `deploymentWords`; `not
   *  recorded` where no run records its deployments). The verdict leaves out each task the
   *  listing rule dropped, and is made with and without the tasks whose listing was not measured
   *  (`listedOutcome`), both in the summary. */
  const writeSummary = (): {
    verdict: LoadVerdict;
    usd: number;
    outside: OutsideListing;
    deployments: string;
  } => {
    const listed = listedOutcome(
      tasks.flatMap((task) =>
        listingDrops.has(task.id)
          ? []
          : [
              {
                id: task.id,
                label: task.label,
                outcome: outcomeOf(task),
                measured: listings.get(task.id)?.state === "measured",
              },
            ]
      ),
      o.runs
    );
    const verdict = { ...listed.verdict, ...setupNote };
    // The runs of the tasks whose capture sends their session's recorded system prompt whole that
    // read a skill outside the recorded listing, from the reads each run records: a count only,
    // which drops no task and changes no verdict.
    const outside = outsideListing(
      recordedListings.map(({ id, recorded }) => ({
        recorded,
        runs: (runsOf.get(id) ?? []).map((run) =>
          run.skillReads === "answered"
            ? run.replies.flatMap((reply) => reply.answers ?? [])
            : undefined
        ),
      }))
    );
    const runs = [...runsOf.values()].flat();
    // Where the runs went, under the tested name, a load command's one arm; a run made before
    // runs recorded their deployments is counted apart.
    const recorded = runs.flatMap((run) =>
      run.deployments === undefined ? [] : [run.deployments]
    );
    const unrecorded = runs.length - recorded.length;
    const spends = tasks.map(spendOf);
    const total = (field: "runsUsd" | "failedRunsUsd" | "hintsUsd" | "usd") =>
      spends.reduce((sum, spend) => sum + spend[field], 0);
    const spend = {
      runsUsd: total("runsUsd"),
      failedRunsUsd: total("failedRunsUsd"),
      hintsUsd: total("hintsUsd"),
      usd: total("usd"),
    };
    writeWhole(
      summaryFile,
      json({
        settings,
        ...setupNote,
        skillDir: given,
        tested: {
          name,
          renamedFrom: settings.renamedFrom,
          changed:
            settings.renamedFrom === null
              ? "nothing"
              : "the frontmatter name and the directory's name, in a scratch copy, and a recorded prompt's listing line of the skill; nothing else that names the skill",
        },
        listed: runs[0]?.listed ?? null,
        tasks: profiled.map(({ task, profile }) => ({
          ...task,
          ...setupNote,
          profile: profile.file,
          profileDigest: profile.digest,
          ...outcomeOf(task),
          spend: spendOf(task),
          dropped: dropped.get(task.id) ?? null,
          listing: listings.get(task.id) ?? null,
          listedUnder: listedUnder.get(task.id) ?? null,
        })),
        listing: listingCounts,
        listedUnder: listedCounts(),
        dispatch: dispatchCounts,
        outsideRecordedListing: {
          tasks: outside.groups,
          runs: outside.runs,
          examined: outside.examined,
          outside: outside.outside,
          bySkill: outside.bySkill,
        },
        verdict,
        verdicts: {
          withUnmeasured: { ...listed.withUnmeasured, ...setupNote },
          withoutUnmeasured: { ...listed.withoutUnmeasured, ...setupNote },
        },
        routeSeed: o.routeSeed ?? "",
        deployments: {
          byName: { [name]: Object.fromEntries(deploymentCounts(recorded)) },
          unrecorded,
        },
        spend,
        peakRssBytes: peak,
        requestLog: o.requestLog,
        updatedAt: new Date().toISOString(),
      })
    );
    const deployments =
      recorded.length === 0 && unrecorded > 0
        ? "not recorded"
        : `${deploymentWords(recorded)}${unrecorded === 0 ? "" : ` (and ${counted(unrecorded, "run")} made before runs recorded their deployments)`}`;
    return { verdict, usd: spend.usd, outside, deployments };
  };
  writeSummary();
  if (listingDrops.size * 2 > tasks.length) {
    throw new Stopped(
      `the listing rule drops ${listingDrops.size} of the ${tasks.length} tasks of ${o.tasksFile}, more than half (${[...listingDrops.keys()].join(", ")}): the rebuilt listing of each misses more than ${MAX_LISTING_CHANGE_PERCENT}% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adds more names than ${MAX_LISTING_CHANGE_PERCENT}% of them, so the test stops before any run, and the architect decides such a load; ${listingCounts.measured} tasks' listings were measured and ${listingCounts.notMeasurable} could not be; written to ${summaryFile}`
    );
  }

  const count = (label: TaskLabel) => tasks.filter((task) => task.label === label).length;
  test.say(
    `the skill ${name} on ${o.host === "omp" ? "Oh My Pi" : "Claude Code"}, model ${settings.model}, thinking ${o.thinking}: ${count("need")} need and ${count("near-miss")} near-miss tasks, ${o.runs} runs each`
  );
  const droppedBy = Object.entries(listingCounts.droppedBy)
    .map(([start, n]) => `${n} ${start}`)
    .join(", ");
  test.say(
    `the listing rule (${listingCounts.summary}): ${listingCounts.measured} tasks' listings measured, ${listingCounts.notMeasurable} not measurable, ${listingCounts.dropped} tasks dropped (${droppedBy}), each missing more than ${MAX_LISTING_CHANGE_PERCENT}% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adding more names than ${MAX_LISTING_CHANGE_PERCENT}% of them, ${listingCounts.fromRecorded} installed names their recorded listing gave them (fromRecorded); ${dispatchCounts.carryBlock} tasks carry the Envoy plugin's block of the skill, ${dispatchCounts.assumed} of them on the assumption that their session had Dispatch`
  );
  const byFile = new Map<string, { profile: TaskProfile; ids: string[] }>();
  for (const { task, profile } of profiled) {
    const entry = byFile.get(profile.file);
    if (entry === undefined) byFile.set(profile.file, { profile, ids: [task.id] });
    else entry.ids.push(task.id);
  }
  for (const { profile, ids } of byFile.values()) {
    test.say(
      `${ids.length === 1 ? "task" : "tasks"} ${ids.join(", ")} run in the context ${profile.profile.context} of the profile ${profile.file} (SHA-256 ${profile.digest})`
    );
    if (o.host === "omp" && pluginInjects({ skill: own, profile: profile.profile })) {
      test.say(
        `the ${PLUGIN_INJECTED.plugin} plugin puts ${own} into each request in the context of ${profile.file}: each capture carries its block, built from the tested text, before the task's text${name === own ? "" : `, the same under the name ${name}`}, so its runs measure whether the agent reads a skill whose text it already holds`
      );
    }
  }
  if (settings.renamedFrom !== null) {
    test.say(
      `${own} is tested under the name ${name}: its frontmatter name and its directory's name are changed in a scratch copy, and so is the name of a recorded prompt's listing line of it, and nothing else that names the skill changes (its own text, its references, other skills and code still say ${own})`
    );
  }
  const failures: string[] = [];
  const listedSaid = new Set<string>();
  const what = (task: Task) =>
    `task ${task.id} (${task.label}, ${task.source === "session" ? "from a session" : "written"})`;
  // Each profile's tasks are captured one by one, then the runs they lack are sent together in
  // one seeded order (`sendRuns`), so no task's runs go as a batch of their own; then the runs
  // again for the runs that are not judged, in an order of their own, until each task has its
  // judged runs or has used its extra runs.
  for (const group of byFile.values()) {
    // Each task's own capture step: its capture, and the read of its capture's record after it
    // (`readCapture`). Its line's total is that, its group's runs and its writes, so no other
    // task's capture is in it.
    const ownMs = new Map<string, number>();
    const ready: ReadyTask[] = [];
    for (const { task, profile } of profiled) {
      if (profile.file !== group.profile.file) continue;
      const drop = dropped.get(task.id);
      if (drop !== undefined) {
        test.say(
          `${what(task)}: ${listingDrops.has(task.id) ? "dropped, not run" : "dropped before, skipped"}: ${drop}`
        );
        continue;
      }
      const outcome = outcomeOf(task);
      if (outcome.toStart === 0) {
        test.say(
          `${what(task)}: ${outcome.complete ? "its run file is complete" : "its run file holds its extra runs"}, skipped: ${countWords(outcome)}: ${verdictWords(task, outcome, o.runs)}`
        );
        continue;
      }
      const startedAt = performance.now();
      try {
        ready.push(await test.captureTask(task, profile));
      } catch (error) {
        // The budget refused a hint call of the task's capture: the test stops as it does when
        // it refuses a run.
        if (error instanceof BudgetSpent) {
          peak = Math.max(peak, o.io.peakRssBytes());
          writeSummary();
          throw new Stopped(
            `the budget is spent, so no new model call starts, and task ${task.id} has no capture; the runs so far are saved under ${o.out}`
          );
        }
        if (error instanceof UnreadableDirectory) {
          // No try reads that directory: the task is dropped, kept so in the summary at once, and
          // a command started again skips it.
          const reason = `${UNREADABLE_DIRECTORY}: ${error.message}`;
          dropped.set(task.id, reason);
          writeSummary();
          test.say(`${what(task)}: dropped: ${reason}`);
        } else {
          failures.push(`task ${task.id}: ${messageFor(error)}`);
          o.io.err(`load: ${what(task)}: ${messageFor(error)}`);
        }
      }
      readCapture(task);
      ownMs.set(task.id, performance.now() - startedAt);
    }
    if (ready.length === 0) continue;
    const writeMs = new Map<string, number>();
    let runsMs = 0;
    let summaryMs = 0;
    let budgetSpent = false;
    for (let batches = 0; ; batches += 1) {
      const batch = ready.flatMap((each) => {
        const { id } = each.task;
        const count = outcomeOf(each.task).toStart;
        const made = runsOf.get(id) ?? [];
        return count > 0 ? [{ each, made, failed: failedOf.get(id) ?? [], count }] : [];
      });
      if (batch.length === 0) break;
      const sent = await test.sendRuns(batch);
      runsMs += sent.runsMs;
      for (const { each, failed: before } of batch) {
        const { id } = each.task;
        const runs = sent.runs.get(id) ?? [];
        const failed = sent.failed.get(id) ?? [];
        const writeStarted = performance.now();
        writeWhole(loadRunFile(o.out, id), runs.map((run) => `${JSON.stringify(run)}\n`).join(""));
        if (failed.length > before.length) {
          writeWhole(
            loadFailedRunFile(o.out, id),
            failed.map((run) => `${JSON.stringify(run)}\n`).join("")
          );
        }
        writeMs.set(id, (writeMs.get(id) ?? 0) + performance.now() - writeStarted);
        runsOf.set(id, runs);
        failedOf.set(id, failed);
      }
      peak = Math.max(peak, o.io.peakRssBytes());
      const summaryStarted = performance.now();
      writeSummary();
      summaryMs += performance.now() - summaryStarted;
      const ids = batch.map(({ each, count }) =>
        batches === 0 ? each.task.id : `${each.task.id} (${count})`
      );
      test.say(
        `the ${batches === 0 ? "runs" : "extra runs, for runs that are not judged,"} of ${ids.length === 1 ? "task" : "tasks"} ${ids.join(", ")} in the context of ${group.profile.file} went in one order (seed ${sent.seed}), each with a session id of its own`
      );
      for (const failure of sent.failures) {
        failures.push(failure);
        o.io.err(`load: ${failure}`);
      }
      budgetSpent = sent.budgetSpent;
      // A run that failed is made again by the next command, not here.
      if (budgetSpent || sent.failures.length > 0) break;
    }
    for (const { task, profile, captured, captureMs, github } of ready) {
      const runs = runsOf.get(task.id) ?? [];
      const outcome = outcomeOf(task);
      const listed = runs[0]?.listed;
      if (listed !== undefined && !listedSaid.has(profile.digest)) {
        listedSaid.add(profile.digest);
        test.say(
          o.host === "omp"
            ? `Oh My Pi lists ${name} under the hint ${JSON.stringify(listed)} in the context of ${profile.file}`
            : `Claude Code lists ${profile.tested.plugin}:${name} as ${JSON.stringify(listed)} in the context of ${profile.file}`
        );
      }
      const phases: TaskPhases = {
        captureMs,
        contextMs: captured.contextMs,
        github,
        runsMs,
        writeMs: (writeMs.get(task.id) ?? 0) + summaryMs,
      };
      const totalMs = (ownMs.get(task.id) ?? captureMs) + phases.runsMs + phases.writeMs;
      const spend = spendOf(task);
      const inDollars = (amount: number) => `$${amount.toFixed(4)}`;
      test.say(
        `${what(task)}: ${countWords(outcome, runs)}: ${verdictWords(task, outcome, o.runs)}; spend at list price: runs ${inDollars(spend.runsUsd)}, ${spend.failedRuns} failed runs ${inDollars(spend.failedRunsUsd)}, hints ${inDollars(spend.hintsUsd)}, all ${inDollars(spend.usd)}; ${phaseWords(phases, totalMs)}`
      );
    }
    if (budgetSpent) {
      throw new Stopped(
        `the budget is spent, so no new model call starts; the runs so far are saved under ${o.out}`
      );
    }
    if (peak > o.memoryLimitBytes) {
      const named = ready.map((each) => each.task.id).join(", ");
      throw new Stopped(
        `the runner's peak resident memory, ${Math.round(peak / 2 ** 20)} MiB, is above its limit of ${Math.round(o.memoryLimitBytes / 2 ** 20)} MiB, so the test stops after ${ready.length === 1 ? "task" : "tasks"} ${named}; their runs are saved under ${o.out}`
      );
    }
  }
  peak = Math.max(peak, o.io.peakRssBytes());
  const { verdict, usd, outside, deployments } = writeSummary();
  const under = listedCounts();
  const noCapture =
    under.none === 0 ? "" : `; ${under.none} tasks have no capture whose record says`;
  test.say(
    o.host === "omp"
      ? `the listing line of ${name}: ${under.recorded} tasks list it under the line their session recorded, ${under.drawn} under a hint of draw ${o.hintDraw}${noCapture}`
      : `the listing line of ${name}: ${under.description} tasks list it under its description, since Claude Code lists no hint${noCapture}`
  );
  test.say(outsideListingWords(outside, "task"));
  test.say(`deployments by name: ${name} ${deployments}`);
  const small =
    count("need") < MIN_TASKS_PER_LABEL || count("near-miss") < MIN_TASKS_PER_LABEL
      ? ` (a load test has at least ${MIN_TASKS_PER_LABEL} need and ${MIN_TASKS_PER_LABEL} near-miss tasks; this one has ${count("need")} and ${count("near-miss")})`
      : "";
  test.say(
    `the skill ${name}: ${verdict.reason}${testSetup === undefined ? "" : ` (${TEST_SETUP_LABEL})`}${small}; the test's runs, its failed runs and its hints so far cost $${usd.toFixed(4)} at list price; written to ${summaryFile}`
  );
  return { failures };
}

/** One run a load audit names: its number, its session id, and the tools of the calls of its
 *  last reply that put it there (the actions; the calls a continued round would leave out; the
 *  undecided calls). */
export interface AuditedLoadRun {
  readonly run: number;
  readonly userId: string | null;
  readonly tools: readonly string[];
}

/** What `auditLoadMixed` finds of one task's saved runs (see its comment). */
export interface TaskAudit {
  readonly task: string;
  readonly runs: number;
  readonly judged: number;
  readonly untouched: number;
  readonly ends: number;
  readonly skillRound: number;
  readonly actions: readonly AuditedLoadRun[];
  readonly continues: readonly AuditedLoadRun[];
  readonly sentBy: readonly AuditedLoadRun[];
  readonly pastLastRound: readonly AuditedLoadRun[];
  readonly unresolved: readonly AuditedLoadRun[];
}

/**
 * The load test's mixed-reply rule (replay.ts's `replyBoundary` under LOAD_RULE) applied to the
 * saved runs of each task the summary under `out` lists (`runs/<task id>.jsonl`), with no model
 * call, no capture and no network: what each run's last reply would come to now. A run that
 * loaded the skill or was sent by another skill, and every Claude Code run, ended before the rule
 * (`untouched`). For each other run, its last reply is rebuilt from its record (ReplyRecord: each
 * call's tool, a read's path, and the command or path LOAD_RULE reads, which a run made before
 * that was recorded lacks, so its `bash` and `write` calls are undecided), and answered as its
 * run would answer it: the tested skill (the run's `name`) from `skillDir`, the other skills of
 * the run's profile file from the copies its task's saved capture names under `cacheDir`
 * (context.ts's `installedCopies`). Each run is counted as ending there as before (`ends`: no
 * read the replay answers, or no call), only reading skill files (`skillRound`), or named with
 * its run number and session id: ending at an action (`actions`), continued (`continues`), sent
 * by another skill whose answer the continued round would make (`sentBy`), getting no verdict
 * past the last round (`pastLastRound`), or getting none for an undecided call (`unresolved`).
 * Throws a refusal for a summary, a run or a profile it cannot read, and an error for a task
 * with no saved capture, or a read of a skill whose copy the cache lacks.
 */
export function auditLoadMixed(o: {
  readonly out: string;
  readonly skillDir: string;
  readonly cacheDir: string;
}): TaskAudit[] {
  const summaryFile = path.join(o.out, "summary.json");
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(summaryFile, "utf8"));
  } catch (error) {
    throw new Refused(`cannot read ${summaryFile}: ${messageFor(error)}`);
  }
  const summary = SavedSummary.safeParse(raw);
  if (!summary.success) throw new Refused(`${summaryFile} is not a load test's summary`);
  const profiles = new Map<string, TaskProfile>();
  return summary.data.tasks.map(({ id }): TaskAudit => {
    const file = loadRunFile(o.out, id);
    const runs = [...jsonLines(file)].map(({ at, value }) => {
      const parsed = LoadRun.safeParse(value);
      if (!parsed.success) {
        throw new Refused(`${file}:${at} is not a load run: ${schemaFaults(parsed.error)}`);
      }
      return parsed.data;
    });
    const audit = {
      task: id,
      runs: runs.length,
      judged: runs.filter(isJudged).length,
      untouched: 0,
      ends: 0,
      skillRound: 0,
      actions: [] as AuditedLoadRun[],
      continues: [] as AuditedLoadRun[],
      sentBy: [] as AuditedLoadRun[],
      pastLastRound: [] as AuditedLoadRun[],
      unresolved: [] as AuditedLoadRun[],
    };
    let copies: readonly InstalledCopy[] | undefined;
    for (const run of runs) {
      const last = run.replies.at(-1);
      if (
        run.host === "claude" ||
        run.outcome === "loaded" ||
        run.outcome === "sent by" ||
        last === undefined
      ) {
        audit.untouched += 1;
        continue;
      }
      if (copies === undefined) {
        const capture = savedCapture(loadCapturePaths(o.out, "omp", id));
        if (capture === undefined) {
          throw new Error(`task ${id} has no saved capture under ${o.out}, which names its copies`);
        }
        copies = installedCopies(capture.record.context, o.cacheDir);
      }
      const profile =
        profiles.get(run.profile) ??
        loadProfile(run.profile, "omp", run.name, run.testSetup, run.testSetup !== undefined);
      profiles.set(run.profile, profile);
      const arm: Arm = {
        name: ARM,
        skill: run.name,
        skillDir: o.skillDir,
        installedDir: path.join("/skills", run.name),
        messages: [],
        profileSkills: [
          ...profile.profile.skills,
          { repo: LOCAL_SKILL_REPO, commit: run.profileDigest, path: o.skillDir, name: run.name },
        ],
      };
      // The reply as its record keeps it: each call's tool and what the rule reads of it.
      const content: ContentBlock[] = last.calls.map((call, index) => ({
        type: "tool_use",
        id: `audited_${index}`,
        name: call.name,
        input:
          call.name === "read" && call.target !== null
            ? { path: call.target }
            : call.argument === undefined
              ? {}
              : { [call.name === "bash" ? RULE_FIELD.bash : RULE_FIELD.write]: call.argument },
      }));
      let boundary: Boundary;
      try {
        boundary = replyBoundary(content, arm, copies, LOAD_RULE);
      } catch (error) {
        throw new Error(`task ${id} run ${run.run}: ${messageFor(error)}`);
      }
      if (boundary.kind === "ends") audit.ends += 1;
      else if (boundary.kind === "skill round") audit.skillRound += 1;
      else {
        const { others } = boundary;
        const named = (part: CallPart) => ({
          run: run.run,
          userId: run.userId,
          tools: partTools(others, part),
        });
        const sender = senderOf(boundary.reads, run.name);
        if (boundary.kind === "acts") audit.actions.push(named("act"));
        else if (boundary.kind === "undecided") audit.unresolved.push(named("undecided"));
        else if (sender !== undefined) audit.sentBy.push(named("preparation"));
        else if (run.replies.length - 1 >= MAX_SKILL_ROUNDS) {
          audit.pastLastRound.push(named("preparation"));
        } else audit.continues.push(named("preparation"));
      }
    }
    return audit;
  });
}
