/**
 * The six arms of a case: `old` (the skill's base text, `--old`), `new` (the
 * cut text, `--new`), `ours` (a skill of ours, `--ours`, under its own name: see below), `none`
 * (no skill), `minus` (a copy of the old directory less every line
 * of the case's baseFile that is the case's line, trimmed, or less every line of any of its files
 * that is `--planted-line`, trimmed, when one is given: a rule written twice is still the rule, so
 * each copy of it goes) and `twin` (the old directory again, built as `old` is: the false-loss
 * control's second copy of the old arm, with its own capture and runs). Only the skill's directory
 * differs between arms: the messages are the case's own but for the text of the skill's files in
 * its read results, and the profile is the case's own, each copy of the skill's source (cases.ts's
 * `testedCopy`: its repository and path, under its own name or a namespace) installed from the
 * arm's directory, which `none` leaves out. A copy of another source that shares the name stays
 * as the profile has it in every arm but `none` and `ours` (below), which leave out every copy of
 * the skill's source. In those two, Oh My Pi 18.18.6 lists a copy under its bare name once it
 * finds no other copy of that name, so for each name that no copy left (ours included, in `ours`)
 * holds bare and some copy holds under a namespace, the copy whose namespace's skills directory
 * Oh My Pi reads first loses its namespace (`relistedSkills`; context.ts gives Oh My Pi `skills/`
 * first, then each namespace's directory in the order of its first copy in the profile). The arm
 * records the name the profile lists each such copy under (`relisted`, as `tools/alpha`), and
 * every other copy keeps its namespace.
 *
 * In each arm but `none` and `ours`, each read result of a `skill://` read of the skill (in the
 * case's `skillReads`) is answered from the arm's file the way the pinned Oh My Pi 18.18.6 answers
 * one: one
 * line `[Skill file: <absolute path>]`, then the file's text less its final newline (F1). The path
 * is never the arm's own directory: it is the one the real result shows, the skill's installed
 * path, so no arm's directory name reaches the model and the lines are the same in every arm. A
 * file the arm lacks gets Oh My Pi's not-found result, in the shape of F1's read of a missing
 * skill (one text part, empty `details`, `isError`): `File not found: <that absolute path>` when
 * the arm has the skill's directory, and `Unknown skill: <skill>\nAvailable: <the profile's other
 * skills>` when it has none. Those texts are the pinned Oh My Pi's own, read from its answers to
 * `read skill://alpha/references/missing.md` and `read skill://gamma` in a scratch profile. In
 * `none`, each read call of the skill and its result are removed, and an assistant message left
 * with no text and no tool call goes too.
 *
 * A whole read of a file of the cut's own copy of the skill by its path on disk, absolute,
 * relative to the session's working directory or under `~` (cases.ts's `pathReadFile` of the
 * case's `pathCopy`, which resolves the path as Oh My Pi does; in `skillReads` too) is answered
 * in each arm but `none` from the arm's file the way the pinned Oh My Pi answers a read of a text
 * file by path (packages/coding-agent/src/tools/read.ts lines 2320-3187 at the tag
 * v18.18.6-sami.20261119-162325 of cybermonklabs/oh-my-pi), at its default settings. It has no
 * `[Skill file: …]` line, which only a `skill://` read gets (lines 2040-2046).
 * In a session that shows hashline rows it is one line `[<path>#<tag>]`, then `<n>:<line>` for
 * each line of the file; in one that shows line numbers, `<n>|<line>`; else the lines alone
 * (`PathRows`, `resolveFileDisplayMode`). `<path>` is the one the real result's own line shows,
 * else the one a read of another file of the copy shows (`PathCopy.shownDir`), else the absolute
 * path, never the arm's own directory; `<tag>` is Oh My Pi's snapshot tag of the arm's text
 * (`snapshotTag`). A line longer than 768 characters (`tools.outputMaxColumns`) shows its first
 * 768 and `…`, and the text ends with Oh My Pi's notice naming the cut lines. A file the arm
 * lacks (but in `ours`, below), and in `none` (which keeps the read call, unlike a `skill://`
 * read's) every file, gets Oh My Pi's not-found result: `Path '<the path as read>' not found`,
 * empty `details`, `isError` (line 2258). These forms, `details` and tags included, are those of
 * recorded reads by path of the pinned Oh My Pi. Oh My Pi shows some files in a form the arms do
 * not rebuild, and buildArm throws naming the case on a read of one: a file that is no Markdown
 * or plain text (a read may summarize it, or show a notebook or an image), one that is not UTF-8
 * text, one of more than 300 lines (`read.defaultLimit`: it shows 300, then the end of each block
 * past them), one whose lines pass the 50 KB a read shows, and one with a line that opens a git
 * conflict block (a warning follows it). `pathFormFault` says which, so the case builder can leave
 * such a case out before it writes it.
 *
 * A case that names no such copy (its session read the skill by `skill://`, and any read of it by
 * path stays as recorded, build.ts's excluded reads) takes for its copy the directory its first
 * `[Skill file: …]` line shows the skill installed in, less the file it read (`pathCopyOf`; none
 * when no such line shows one), the same in every arm. A new whole read by path of a file under
 * that directory, or one of one line range, is answered as a new read of the cut's own copy is
 * (`pathFile`, below): from the arm's file, at the path read, its hashline line naming the file
 * under the directory a recorded read of a file there shows, else the absolute one, and in `none`,
 * which installs no copy of the skill, Oh My Pi's not-found result. Such a copy names no working
 * or home directory, so a read of it by a relative or `~` path is not answered.
 *
 * `ours` is a skill whose frontmatter name (and directory name) may differ from the case's skill
 * (`ArmDirs.ours` gives its directory and its name). Its profile is the case's less each copy of
 * their skill's source, as `none` leaves them out, with ours under our name in the place of the
 * copy listed under theirs; where the profile already lists a skill of another source under our
 * name, the `--ours` text replaces that entry, so no name is listed twice (context.ts refuses one
 * installed twice), and the arm records that it replaced it. Then it lists each name as `none`
 * does (above, `relistedSkills` over the arm's skills, ours included): a copy of another source
 * listed as `<namespace>/<their name>` loses its namespace where no copy left holds their name
 * bare, and the arm records it (`relisted`); ours, bare in `skills/`, which Oh My Pi reads first,
 * keeps our name from a copy that holds it under a namespace. Its installed directory is theirs
 * with our name as its last part. Each history read of a file of theirs becomes a read of
 * `skill://<ours>` at the same path inside the skill (the call's `path` argument), answered from
 * our directory at our installed directory, when ours has that file; a whole read of a file of
 * the cut's own copy by path keeps its call and its path, and is answered from our file at the
 * same path inside the skill in the form above. When ours lacks the file, the read call and its
 * result are removed as `none` removes a `skill://` read's, and the arm records the read's path
 * inside the skill. So a renamed unchanged copy of their skill gives old's texts but for the name
 * (and a read by path's snapshot tag). A reply's `skill://` read of their skill reads the copy of
 * another source the arm lists under their name, where it lists one (`caseNameCopy`, below), and
 * else gets Oh My Pi's unknown-skill text (`unknownSkill`): the arm does not install theirs; a
 * reply's whole read by path of the cut's own copy, or of the installed directory a case that names
 * no copy takes for one, is answered from our file at that path, or as not found where ours lacks
 * it.
 *
 * `skillFile(arm, file)` answers one new `skill://` read the same way, at the arm's
 * `installedDir`, and `pathFile(arm, target)` one new read by path of a file of the arm's copy
 * (`Arm.pathCopy`: the cut's own, or the installed directory), its hashline line naming the file
 * under `PathCopy.shownDir`; replay.ts answers a reply's reads with them. cases.ts's
 * `skillReadFile(arm.skill, target)` and `pathReadFile(arm, target)` say which file a read reads.
 *
 * `otherSkillRead(arm, copies, target)` answers one new read of a file of another skill, the
 * same text in every arm. A skill the profile lists is read from its installed copy (`copies`,
 * the cache entries the capture installed, context.ts's `installedCopies`), or from the arm's
 * directory for a copy of the case's skill's source under another name: a `skill://<name>[/<file>]`
 * read under a `[Skill file: <path>]` line whose directory is the one the case's own reads of
 * that skill show (`arm.skillDirs`), else the directory beside the case's skill's installed
 * directory; a whole read by path (a path that ends `/<name>/SKILL.md`, or holds
 * `skills/<name>/references/<file>`, as count.sql and build.ts find a skill's read by path) in Oh
 * My Pi's form of a read by path, its rows as the session shows them (`arm.pathRows`). In `none`,
 * and in `ours` once their copies are gone, a `skill://<skill>[/<file>]` read of the case's skill
 * is answered so from the copy of another source the profile lists under the skill's name
 * (`caseNameCopy`), where it lists one. A `skill://` read of a skill the profile does not list
 * gets Oh My Pi's unknown-skill result, the text of `skill-protocol.ts`'s resolver in the pinned
 * binary: `Unknown skill: <name>`, then `Available:` and the names the session installs joined by
 * `, ` (`none` for no name).
 *
 * A skill can reach a session other than by its listing line and its reads, and each arm puts its
 * own text there too (`Injection`, `arm.injections`):
 *
 * - **A plugin's block.** The Envoy plugin puts dispatch-first into every request of a session
 *   with Dispatch (PLUGIN_INJECTED; packages/pi-envoy/extensions/envoy.ts): a user message of one
 *   text part, stamped 0, after the leading compaction summaries, unless the message there already
 *   opens with the marker (pi-envoy's `withDispatchFirst`), its text `readDispatchFirstContext` of
 *   the plugin's own `skills/dispatch-first/SKILL.md` (packages/envoy-client/src/dispatch-first.ts),
 *   read as the plugin loads, so a plugin that finds no such file does not load. Oh My Pi stores
 *   nothing a `context` handler returns, so no transcript holds the block, and a capture runs with
 *   Dispatch off (capture.ts). So for a case of dispatch-first whose profile installs the plugin,
 *   each arm but `none` puts the block, from the arm's SKILL.md through the plugin's own function,
 *   at that place in its messages (taking out any block there), and `none` puts none. Whether the
 *   session had Dispatch is taken from the case where the case says (`dispatchOf`): a Legion
 *   role's session, or a subagent of one, had it, and so did a session whose messages hold the
 *   dispatch-first block or a Dispatch call; a dev box's session that shows neither is taken to
 *   have had it. Each arm carries that answer and why (`arm.dispatch`, which a capture records),
 *   and the block goes in on each answer. context.ts installs a hidden stand-in of the file where
 *   the arm has no copy under that name (`none`), so the plugin loads in every arm and lists
 *   nothing more.
 * - **An autoloaded block.** Oh My Pi gives a subagent each skill its agent definition's
 *   `autoloadSkills` names as a `custom` message of type `skill-prompt` (SKILL_PROMPT) before its
 *   task, `display: false`, its details the skill's name and path (packages/coding-agent/src/task/
 *   executor.ts lines 5573-5607 at the pin, the tag v18.18.6-sami.20261119-162325 of
 *   cybermonklabs/oh-my-pi), its text `prompt.render` of prompts/skills/autoload.md trimmed
 *   (extensibility/skills.ts lines 968-976): the SKILL.md less its frontmatter, trimmed (lines
 *   901-906), a blank line, `---`, a blank line and `Skill: <path>`, all of it then put through
 *   Oh My Pi's `format` (packages/utils/src/prompt.ts lines 192-308 and 544-548; `formatPrompt`).
 *   The transcript keeps that message, so the case's messages hold it: each arm but `none`
 *   rebuilds its text from the arm's SKILL.md under the recorded path (`autoloadedText`), and
 *   `none` leaves the message out.
 *
 * In `ours`, each block carries our text in their skill's place: the plugin's block is the
 * plugin's own function of our SKILL.md, and an autoloaded block is rebuilt from our SKILL.md
 * under the recorded path with our name as its skill directory's last part, its details naming
 * our skill and that path.
 *
 * A case whose messages hold a block of its skill that the runner cannot rebuild for each arm
 * (`injectionFault`: a `skill-prompt` message of the skill in another form, such as one the user
 * invoked with `/skill:`, or a message that opens with the plugin's marker away from where the
 * plugin puts its block) is refused: buildArm throws InjectionRefused naming it.
 */
import { isUtf8 } from "node:buffer";
import {
  cpSync,
  existsSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  type Stats,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import {
  DISPATCH_FIRST_MARKER,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import {
  AT_COMMAND,
  readTargets,
  type SessionMessage,
  skillFileHeader,
  skillHeaderPath,
  stripSkillHeader,
  withoutBodies,
} from "../skill-scenarios/transcript";
import {
  type Case,
  listedName,
  type ProfileSkill,
  pathReadFile,
  resolvedReadPath,
  skillReadFile,
  testedCopy,
  testedSource,
  textLines,
  URL_TARGET,
  wholeReadTarget,
} from "./cases";
import { ompText, splitFrontmatter } from "./common";
import type { Arm as ScoreArm } from "./score";

/** The name of an arm, as run records carry it. */
export type ArmName = ScoreArm;

/** The name a profile gives the Envoy plugin: its directory's (build.ts names each plugin after
 *  its directory, `packages/pi-envoy` in cybermonklabs/legion). */
export const ENVOY_PLUGIN = "pi-envoy";

/** The skill a plugin puts into a session's requests itself, under the name the plugin reads its
 *  file by, and that plugin (see the file's header). */
export const PLUGIN_INJECTED = { skill: "dispatch-first", plugin: ENVOY_PLUGIN } as const;

/** The custom message type of a skill's text Oh My Pi puts into a session
 *  (SKILL_PROMPT_MESSAGE_TYPE, packages/tui/src/chat/messages.ts line 21 at the pin). */
const SKILL_PROMPT = "skill-prompt";

/** A block of the case's skill that reaches its session beside the listing line and the reads,
 *  and where the arm's messages hold it (`at`; null for `none`, which holds none): one a plugin
 *  puts into each request (`plugin`, as a profile names it; `name`, the skill's name the plugin
 *  reads its file under), or one Oh My Pi autoloads into a subagent (`path`, the SKILL.md path the
 *  block names, as recorded). */
export type Injection =
  | {
      readonly by: "plugin";
      readonly plugin: string;
      readonly name: string;
      readonly at: number | null;
    }
  | { readonly by: "autoload"; readonly path: string; readonly at: number | null };

/** What buildArm, and a capture, throw for a case that holds a block of its skill the runner
 *  cannot give each arm's text, its message naming the case: behaviour refuses the case on it
 *  (cli.ts). `kind` is `autoload-no-block` for a capture's: a subagent whose agent autoloads the
 *  skill and whose messages hold no autoloaded block of it (capture.ts); buildArm's sets none, and
 *  `injectionFaults` gives the kind of each of its faults. */
export class InjectionRefused extends Error {
  constructor(
    message: string,
    readonly kind?: "autoload-no-block"
  ) {
    super(message);
  }
}

/** Whether a case's session had Dispatch, which the Envoy plugin's block rests on, where that
 *  answer comes from, and why (`dispatchOf`): `legion-role` for a Legion role's session or a
 *  subagent of one; `transcript` for a session whose messages hold the dispatch-first block or a
 *  Dispatch call; `assumed` for a dev box's session that shows neither, which the runner takes to
 *  have had Dispatch. */
export interface DispatchAnswer {
  readonly from: "legion-role" | "transcript" | "assumed";
  readonly why: string;
}

/** How a session shows the rows of a file it read by path (`resolveFileDisplayMode`,
 *  packages/coding-agent/src/utils/file-display-mode.ts at the pin): `hashline`, a
 *  `[<path>#<tag>]` line, then `<n>:` before each line (the edit tool on, in Oh My Pi's default
 *  edit mode); `numbered`, `<n>|` before each line (the `readLineNumbers` setting); `plain`, the
 *  lines alone. */
export type PathRows = "hashline" | "numbered" | "plain";

/** The copy of a case's skill whose files a read by path reads (see the file's header): the cut's
 *  own copy as the session read it by path (cases.ts's `pathCopy`), or, for a case that names none,
 *  the directory its `skill://` reads show the skill installed in (`pathCopyOf`). `dir`, its
 *  absolute directory, and `cwd` and `home`, what resolves a read's relative or `~` path to it
 *  (cases.ts's `pathCopy` and `resolvedReadPath`; absent for an installed directory); `shownDir`,
 *  the directory a hashline line shows its files under (`""` when it shows them relative to the
 *  copy itself); `rows`, how a new read of it shows its rows. */
export interface PathCopy {
  readonly dir: string;
  readonly cwd?: string;
  readonly home?: string;
  readonly shownDir: string;
  readonly rows: PathRows;
}

/** One arm of a case. */
export interface Arm {
  readonly name: ArmName;
  /** The skill the arm installs, as `skill://<skill>` names it: the case's skill, or in `ours`
   *  our skill's name. */
  readonly skill: string;
  /** The arm's directory of the skill, absolute, or undefined for `none`. The model never sees
   *  it. */
  readonly skillDir: string | undefined;
  /** The skill's directory as the model sees it, the same in every arm of a case but `ours`: the
   * installed directory the case's first `[Skill file: …]` line of the skill shows, or
   * `/skills/<skill>` when the case has none; in `ours`, that directory with our name as its last
   * part. */
  readonly installedDir: string;
  /** The case's messages, as this arm gives them. */
  readonly messages: SessionMessage[];
  /** The skills the arm's profile installs: the case's own, less each copy of the case's skill's
   * source for `none` (cases.ts's `testedCopy`), and for `ours` less those too, with ours in the
   * place of the copy listed under the case's skill or of the skill listed under our name; in
   * both, a copy of another source can lose its namespace (`relisted`). Each copy of the arm's
   * skill's source (`testedCopy(profileSkills, skill)`) is installed from `skillDir`. */
  readonly profileSkills: ProfileSkill[];
  /** The name the case's profile lists each copy under that the arm lists under its bare name
   *  (`tools/alpha` for a copy `none` or `ours` lists as `alpha`; `relistedSkills`). Absent where
   *  the arm lists every copy as the profile does. */
  readonly relisted?: readonly string[];
  /** Each block of the skill that reaches the session beside its listing line and its reads, and
   *  where this arm holds it (Injection). Absent, or empty, when the case has none. */
  readonly injections?: readonly Injection[];
  /** Whether the case's session had Dispatch, and why (`dispatchOf`): what the plugin's block of
   *  `injections` rests on. Absent for an arm built from no case (a load test's). */
  readonly dispatch?: DispatchAnswer;
  /** In `ours` only: the case's skill (`theirs`), which the arm does not install; whether our text
   *  replaced a skill of our name that the profile held (`replacedOwn`); and the path inside the
   *  skill of each history read of theirs removed because ours has no file there
   *  (`removedReads`), in the history's order. */
  readonly ours?: {
    readonly theirs: string;
    readonly replacedOwn: boolean;
    readonly removedReads: readonly string[];
  };
  /** The copy of the skill a read by path reads, the same in every arm (`pathCopyOf`): the cut's
   *  own copy the case's session read by path, else the installed directory its `skill://` reads
   *  show. Absent when the case names no copy and its reads show no installed directory. */
  readonly pathCopy?: PathCopy;
  /** How a new read by path shows its rows in the case's session (`pathRowsOf`). Absent for an
   *  arm built from no case (a load test's), whose reads by path show hashline rows. */
  readonly pathRows?: PathRows;
  /** The installed directory of each other skill of the profile that the case's own `skill://`
   *  reads of it show (the first `[Skill file: …]` line of a read of it), by the name the profile
   *  lists it under (`skillDirsOf`). Absent, or without a name, where the case shows none. */
  readonly skillDirs?: Readonly<Record<string, string>>;
}

/** How a new read by path shows its rows in `arm`: as a new read of the case's own copy does
 *  (`pathCopy.rows`), else as the case's session shows them (`pathRows`), else hashline. */
export function newReadRows(arm: Pick<Arm, "pathCopy" | "pathRows">): PathRows {
  return arm.pathCopy?.rows ?? arm.pathRows ?? "hashline";
}

/** The directories the arms are built from: each is the skill's own directory, SKILL.md at its
 * top. `new` is given with `--new`, and `ours`, with its name, with `--ours`. */
export interface ArmDirs {
  readonly old: string;
  readonly new?: string;
  readonly ours?: { readonly dir: string; readonly name: string };
  /** The line `minus` leaves out in place of the case's line. */
  readonly plantedLine?: string;
}

/** Oh My Pi's result for a `skill://` read of `skill` in a session whose skills are the arm's
 *  profile's: the unknown-skill error of `skill-protocol.ts`'s resolver in the pinned binary,
 *  `Unknown skill: <skill>`, then `Available: ` and the listed names joined by `, `, or `none`.
 *  replay.ts answers so a read of their skill in the arm of ours, which does not install it,
 *  where the arm lists no copy of another source under their name (`caseNameCopy`). */
export function unknownSkill(arm: Arm, skill: string): { text: string; isError: boolean } {
  const names = arm.profileSkills.map((each) => listedName(each));
  const available = names.length > 0 ? names.join(", ") : "none";
  return { text: `Unknown skill: ${skill}\nAvailable: ${available}`, isError: true };
}

/** The answer to a `skill://` read of `file` (a path inside a skill's directory) from the skill
 *  directory `dir`, shown at `shown`: the header and the text, or the not-found text with
 *  `isError`. */
function fileAnswer(dir: string, file: string, shown: string): { text: string; isError: boolean } {
  const { full, stats } = armFile(dir, file);
  if (stats === undefined) return { text: `File not found: ${shown}`, isError: true };
  if (!stats.isFile()) throw new Error(`${full} is not a file: an arm answers reads of files only`);
  const text = readFileSync(full, "utf8").replace(/\n$/, "");
  return { text: `${skillFileHeader(shown)}\n${text}`, isError: false };
}

/** The answer to a read of `file` (a path inside the skill's directory) from the arm's files,
 * shown at `shown`: the header and the text, or the not-found text with `isError`; for `none`,
 * the unknown-skill result. */
function answer(arm: Arm, file: string, shown: string): { text: string; isError: boolean } {
  if (arm.skillDir === undefined) return unknownSkill(arm, arm.skill);
  return fileAnswer(arm.skillDir, file, shown);
}

/** The file `file` (a path inside the skill's directory) of the arm's directory `dir`, absolute,
 *  and what is there (undefined: nothing). Throws when `file` leaves the directory. */
function armFile(dir: string, file: string): { full: string; stats: Stats | undefined } {
  const full = path.resolve(dir, file);
  const inside = path.relative(dir, full);
  if (inside === ".." || inside.startsWith(`..${path.sep}`) || path.isAbsolute(inside)) {
    throw new Error(`${file} is not a path inside the skill directory ${dir}`);
  }
  return { full, stats: statSync(full, { throwIfNoEntry: false }) };
}

/** The answer to a new read of `file` (a path inside the skill's directory) from the arm's files,
 * at the arm's `installedDir`: the header and the text, or the not-found text with `isError`. */
export function skillFile(arm: Arm, file: string): { text: string; isError: boolean } {
  return answer(arm, file, path.join(arm.installedDir, file));
}

/** The answer to a `skill://` read of `file` (a path inside a skill's directory) with the line
 *  range `range` from the skill directory `dir`, shown at `shown` (`skillRangeText`), or the
 *  not-found text with `isError`; undefined for a range past the file's end. */
function rangeAnswer(
  dir: string,
  file: string,
  shown: string,
  range: { readonly first: number; readonly last: number | undefined }
): { text: string; isError: boolean } | undefined {
  const { full, stats } = armFile(dir, file);
  if (stats === undefined) return { text: `File not found: ${shown}`, isError: true };
  if (!stats.isFile()) throw new Error(`${full} is not a file: an arm answers reads of files only`);
  const text = skillRangeText(readFileSync(full, "utf8"), shown, range);
  return text === undefined ? undefined : { text, isError: false };
}

/** The answer to a new `skill://` read of `file` with the line range `range` from the arm's files,
 *  at the arm's `installedDir` (`rangeAnswer`); for `none`, the unknown-skill result; undefined
 *  for a range past the file's end. */
export function skillFileRange(
  arm: Arm,
  file: string,
  range: { readonly first: number; readonly last: number | undefined }
): { text: string; isError: boolean } | undefined {
  if (arm.skillDir === undefined) return unknownSkill(arm, arm.skill);
  return rangeAnswer(arm.skillDir, file, path.join(arm.installedDir, file), range);
}

/** A read result of the case's messages answered from the arm's files at `shown`: F1's shape of a
 * read of a skill file, or of a read that failed. */
function answered(
  message: SessionMessage,
  arm: Arm,
  read: { file: string; target: string; shown: string }
): SessionMessage {
  const { text, isError } = answer(arm, read.file, read.shown);
  if (isError) return { ...message, content: [{ type: "text", text }], details: {}, isError };
  const body = stripSkillHeader(text);
  const lineNumbers = textLines(body).map((_, index) => index + 1);
  return {
    ...message,
    content: [{ type: "text", text }],
    details: {
      resolvedPath: read.shown,
      totalLines: lineNumbers.length,
      rowPrefixes: false,
      displayContent: { text: body, startLine: 1, lineNumbers },
      meta: { source: { type: "internal", value: read.target } },
      ...(read.file.endsWith(".md") ? { contentType: "text/markdown" } : {}),
    },
    isError,
  };
}

/** The result of the read call `toolCallId` of `read.target`, which reads `read.file` (a path
 *  inside the skill's directory), answered from the arm's files at `read.shown` as every arm but
 *  `none` answers a history read: F1's shape of a read of a skill file, or of a read that failed.
 *  `build-cases --written` answers a written case's read so, from `--old`. */
export function readResult(
  arm: Arm,
  read: { file: string; target: string; shown: string },
  toolCallId: string
): SessionMessage {
  return answered({ role: "toolResult", toolCallId, toolName: "read" }, arm, read);
}

/** Oh My Pi's defaults that shape a read by path (packages/coding-agent/src/tools/settings.ts at
 *  the pin): the lines one read shows (`read.defaultLimit`, lines 161-178), the characters it
 *  shows of each line (`tools.outputMaxColumns`, lines 85-105), and the bytes of a result past
 *  which it is saved as an artifact (`tools.artifactSpillThreshold`, lines 13-37), which also
 *  bounds the lines a read shows; a page of a file may pass it by INLINE_SLACK
 *  (packages/coding-agent/src/tools/output-meta.ts line 537). */
const READ_LINES = 300;
const READ_COLUMNS = 768;
const SPILL_BYTES = 50 * 1024;
const INLINE_SLACK = 2 * 1024;

/** The names of the files Oh My Pi reads as prose, which a read never summarizes at its default
 *  settings (read-summary.ts's `isProseSummaryPath` at the pin: Markdown, as
 *  packages/tui/src/lang-from-path.ts lines 104-109 names it, and `.txt`). */
const PROSE_FILE = /\.(md|markdown|mdx|mdc|mkd|mdown|txt)$/i;
/** The names of the files a read at the pin shows in a form of their own whatever their bytes: a
 *  notebook, an SVG image, and the documents markit converts (read.ts lines 2380-2445;
 *  utils/markit.ts line 22). */
const OWN_FORM_FILE = /\.(ipynb|svg|pdf|docx|pptx|xlsx|epub)$/i;
/** The lines from which a read at the pin may summarize a file that is not prose
 *  (`read.summarize.minTotalLines`, settings.ts lines 241-245; read-summary.ts's `trySummarize`
 *  counts a file's newlines and one more, `countTextLines`). */
const SUMMARY_MIN_LINES = 100;
/** The lines a read with a line range shows around it (`RANGE_LEADING_CONTEXT_LINES` and
 *  `RANGE_TRAILING_CONTEXT_LINES`, read-format.ts lines 287-288 at the pin): one before a range
 *  that does not start at line 1, and three after a range with an end. */
const RANGE_BEFORE = 1;
const RANGE_AFTER = 3;

/** A line that opens a git conflict block, as Oh My Pi's scanConflictLines finds one
 *  (packages/coding-agent/src/tools/conflict-detect.ts at the pin). */
const CONFLICT_OPENER = /^<<<<<<<(?: |\r?$)/;

/** The bytes of a file's lines a whole read of it shows, each cut line counted as shown, with a
 *  newline between two (read.ts lines 2663-2672 at the pin, whose `Math.min` of its two bounds
 *  is always this one at its defaults): 50 KB, less four bytes for each of the 300 lines when the
 *  rows show their numbers. */
function pageBytes(rows: PathRows): number {
  const numbers = rows === "plain" ? 0 : String(READ_LINES).length + 1;
  return SPILL_BYTES - READ_LINES * numbers;
}

/** Oh My Pi's snapshot tag of a file's text, the `<tag>` of a hashline line (`file_hash`,
 *  crates/pi-edit/src/store.rs lines 70-91 at the pin): xxHash32, seed 0, of the text with the
 *  spaces, tabs and carriage returns at the end of each line left out, its low 16 bits as four
 *  uppercase hex digits. Oh My Pi hashes the text with a leading byte order mark left out and
 *  each CRLF or CR as LF, which its caller does first (`fileTag`). */
export function snapshotTag(text: string): string {
  const kept = text
    .split("\n")
    .map((line) => line.replace(/[ \t\r]+$/, ""))
    .join("\n");
  return (Bun.hash.xxHash32(kept, 0) & 0xffff).toString(16).toUpperCase().padStart(4, "0");
}

/** Oh My Pi's snapshot tag of a file's text `raw` as a read or a grep shows it: `snapshotTag` of
 *  the text less a leading byte order mark, each CRLF or CR as LF. */
export function fileTag(raw: string): string {
  return snapshotTag((raw.startsWith("\uFEFF") ? raw.slice(1) : raw).replace(/\r\n?/g, "\n"));
}

/** A read by path, answered: its text, whether it failed, and the tool's record. replay.ts answers
 *  a written case's read of a file of its checkout copy so (checkout.ts), where `wholeText` marks a
 *  file the pin may show summarized answered with its whole text (`pathPage`). */
export interface PathAnswer {
  readonly text: string;
  readonly isError: boolean;
  readonly details: Record<string, unknown>;
  readonly wholeText?: true;
}

/** The row of line `n` of a read by path, `line` as shown, in a session that shows its rows as
 *  `rows`: `<n>:<line>`, `<n>|<line>`, or the line alone. */
function pathRow(n: number, line: string, rows: PathRows): string {
  if (rows === "hashline") return `${n}:${line}`;
  return rows === "numbered" ? `${n}|${line}` : line;
}

/** A file's text as a whole read by path shows it: the decoded text, its lines (a final newline
 *  ends the last one), each line shown (at most READ_COLUMNS characters, then `…`; the first
 *  READ_LINES lines of a longer file, a checkout read only), the first and last line cut so, or
 *  undefined when none is, how many lines of the file are left after those shown (`more`), and
 *  whether it is a file the pin may show summarized, given unsummarized (`wholeText`, a checkout
 *  read only). */
interface PathPage {
  readonly raw: string;
  readonly lines: readonly string[];
  readonly shownLines: readonly string[];
  readonly cut: { readonly first: number; readonly last: number } | undefined;
  readonly more: number;
  readonly wholeText: boolean;
}

/** The page of `file` (a path inside the skill's directory) whose bytes are `bytes`, as a whole
 *  read by path shows it in a session that shows its rows as `rows`, or why Oh My Pi shows it in
 *  a form the arms do not rebuild (`pathFormFault`). With `checkoutRead` (a written case's read of
 *  its checkout, checkout.ts), a file that is no prose is rebuilt too, but a file of OWN_FORM_FILE;
 *  one of SUMMARY_MIN_LINES lines or more, which the pin may summarize (read.ts lines 2519-2533:
 *  a code file whose parse elides a body, which the runner cannot rebuild), is given unsummarized
 *  in place of that summary, `wholeText` set; and a file of more than READ_LINES lines is given as
 *  the pin pages it unsummarized (read.defaultLimit, read.ts lines 2652-3030): its first
 *  READ_LINES lines, then the count of the lines left (`more`). */
function pathPage(
  file: string,
  bytes: Uint8Array,
  rows: PathRows,
  checkoutRead = false
): PathPage | string {
  let wholeText = false;
  if (!PROSE_FILE.test(file)) {
    if (!checkoutRead) {
      return `${file} is no Markdown or plain-text file, the only kinds whose read by path the arms rebuild`;
    }
    if (OWN_FORM_FILE.test(file)) return `${file} is a file a read shows in a form of its own`;
    const newlines = bytes.reduce((count, byte) => (byte === 10 ? count + 1 : count), 0);
    wholeText = bytes.length > 0 && newlines + 1 >= SUMMARY_MIN_LINES;
  }
  if (bytes.includes(0) || !isUtf8(bytes)) return `${file} is not UTF-8 text`;
  // The lines as Oh My Pi shows them: the bytes decoded as they are, a final newline ending the
  // last line (read.ts lines 266-291 at the pin).
  const raw = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength).toString("utf8");
  const lines = textLines(raw);
  if (lines.length > READ_LINES && !checkoutRead) {
    return `${file} has ${lines.length} lines, and a read by path shows ${READ_LINES}, then the end of each block past them`;
  }
  const paged = lines.slice(0, READ_LINES);
  const opener = paged.findIndex((line) => CONFLICT_OPENER.test(line));
  if (opener !== -1) {
    return `line ${opener + 1} of ${file} opens a git conflict block, which a read by path follows with a warning`;
  }
  // Each line as shown; the cut lines, the first one's bytes, and the bytes the page counts: each
  // line as shown, and a newline between two.
  const shownLines: string[] = [];
  let cut: { first: number; last: number; firstBytes: number } | undefined;
  let spent = -1;
  for (const [index, line] of paged.entries()) {
    const shown = line.length <= READ_COLUMNS ? line : `${line.slice(0, READ_COLUMNS)}…`;
    if (shown !== line) {
      cut = cut ?? { first: index + 1, last: index + 1, firstBytes: Buffer.byteLength(line) };
      cut.last = index + 1;
    }
    spent += Buffer.byteLength(shown) + 1;
    shownLines.push(shown);
  }
  if (spent > pageBytes(rows)) {
    return `the lines of ${file} a read by path shows come to ${spent} bytes, past the ${pageBytes(rows)} it shows`;
  }
  if (cut !== undefined && cut.firstBytes > SPILL_BYTES) {
    return `line ${cut.first} of ${file} is ${cut.firstBytes} bytes, and a read by path names a byte cap for it`;
  }
  return {
    raw,
    lines,
    shownLines,
    cut: cut === undefined ? undefined : { first: cut.first, last: cut.last },
    more: lines.length - paged.length,
    wholeText,
  };
}

/** Why Oh My Pi shows a whole read by path of `file` (a path inside the skill's directory) whose
 *  bytes are `bytes` in a form the arms do not rebuild, or undefined when they rebuild it (see the
 *  file's header): a file that is no Markdown or plain text, one that is not UTF-8 text or holds a
 *  NUL byte, one of more than 300 lines, one whose lines pass the bytes a read shows, one whose
 *  first cut line passes 50 KB, and one with a line that opens a git conflict block. `rows` is how
 *  the session shows its rows; left out, the bytes a read shows are those of rows that show their
 *  numbers, the fewer, so a caller that cannot tell (the case builder, before it writes a case)
 *  refuses every file buildArm refuses. buildArm throws, naming the case, on each fault, and on a
 *  page whose hashline path alone is some 2 KB long, which Oh My Pi saves as an artifact. */
export function pathFormFault(
  file: string,
  bytes: Uint8Array,
  rows: PathRows = "hashline"
): string | undefined {
  const page = pathPage(file, bytes, rows);
  return typeof page === "string" ? page : undefined;
}

/** Oh My Pi's result for a whole read by the path `target` of `file` (a path inside the skill's
 *  directory) of the arm's directory `skillDir` (undefined for `none`), the absolute path it opens
 *  being `source`, its hashline line naming `shown` and its rows shown as `rows` (see the file's
 *  header; read.ts lines 2320-3187 at the pin). Returns why the arms do not rebuild it instead
 *  when Oh My Pi shows the file in another form (`pathFormFault`, or a page it saves as an
 *  artifact). checkout.ts answers a read of a file of a written case's checkout copy with it,
 *  `skillDir` the copy, `file` the path inside it and `checkoutRead` set (`pathPage`). */
export function pathAnswer(
  skillDir: string | undefined,
  read: {
    file: string;
    target: string;
    source: string;
    shown: string;
    rows: PathRows;
    checkoutRead?: boolean;
  }
): PathAnswer | string {
  const notFound = { text: `Path '${read.target}' not found`, isError: true, details: {} };
  if (skillDir === undefined) return notFound;
  const { full, stats } = armFile(skillDir, read.file);
  if (stats === undefined) return notFound;
  if (!stats.isFile()) throw new Error(`${full} is not a file: an arm answers reads of files only`);
  const bytes = readFileSync(full);
  const page = pathPage(read.file, bytes, read.rows, read.checkoutRead === true);
  if (typeof page === "string") return page;
  const { raw, lines, shownLines, cut, more } = page;
  const numbered = shownLines.map((line, index) => pathRow(index + 1, line, read.rows));
  const shown =
    read.rows === "hashline"
      ? `[${read.shown}#${fileTag(raw)}]\n${numbered.join("\n")}`
      : numbered.join("\n");
  if (Buffer.byteLength(shown) > SPILL_BYTES + INLINE_SLACK) {
    return `a read by path of ${read.file} is saved as an artifact`;
  }
  // A paged file's count of the lines left, then the column note (read.ts lines 3022-3028, and
  // its result builder's limits after them).
  const left =
    more === 0 ? "" : `\n\n[${more} more lines in file. Use :${shownLines.length + 1} to continue]`;
  const notice =
    cut === undefined
      ? ""
      : `\n\n[Some lines truncated to ${READ_COLUMNS} chars. Use :raw:${cut.first}-${cut.first} to read line ${cut.first} whole${cut.last > cut.first ? `; cut lines run to line ${cut.last}, each read the same way` : ""}]`;
  return {
    text: `${shown}${left}${notice}`,
    isError: false,
    details: {
      totalLines: lines.length,
      rowPrefixes: read.rows !== "plain",
      displayContent: {
        text: shownLines.join("\n"),
        startLine: 1,
        lineNumbers: shownLines.map((_, index) => index + 1),
      },
      fileSize: bytes.length,
      meta: {
        source: { type: "path", value: read.source },
        pagedSource: true,
        ...(cut === undefined
          ? {}
          : {
              limits: {
                columnTruncated: {
                  maxColumn: READ_COLUMNS,
                  unit: "chars",
                  lines: { first: cut.first, last: cut.last },
                  selectorBase: "",
                },
              },
            }),
      },
    },
    ...(page.wholeText ? { wholeText: true as const } : {}),
  };
}

/** A read target's selector (the text after a `:` that follows its last `/`): the target less it,
 *  the selector, and the lines it names when it is one line range of the pin's grammar
 *  (read-selector.ts's `parseSel` and line-ranges.ts's `parseLineRanges` at the pin): `N-M` or
 *  `N..M` (M not below N), `N+K` (K lines from N), and `N`, `N-` or `N..` (from line N to the
 *  end), each number with an optional `L` before it, N from 1; `range` undefined for any other
 *  selector (`raw`, a list, `-N`, `conflicts`, …). Undefined when the target holds no selector. */
export function readRange(target: string):
  | {
      readonly path: string;
      readonly selector: string;
      readonly range?: { readonly first: number; readonly last: number | undefined };
    }
  | undefined {
  const colon = target.indexOf(":", target.lastIndexOf("/") + 1);
  if (colon < 0) return undefined;
  const read = { path: target.slice(0, colon), selector: target.slice(colon + 1) };
  const match = /^L?(\d+)(?:(\.\.|[-+])L?(\d+)?)?$/i.exec(read.selector);
  const first = Number(match?.[1] ?? 0);
  const to = match?.[3] === undefined ? undefined : Number(match[3]);
  if (match === null || first < 1) return read;
  if (match[2] === "+")
    return to === undefined || to < 1 ? read : { ...read, range: { first, last: first + to - 1 } };
  if (to !== undefined && to < first) return read;
  return { ...read, range: { first, last: to } };
}

/** The lines a read with the line range `first` to `last` (to the end when undefined) shows of a
 *  text of `count` lines, at the pin, from index `start` to before `end`: RANGE_BEFORE line before
 *  a range that does not start at line 1, RANGE_AFTER lines after a range with an end, and, for a
 *  range with no end, `openLines` lines from `first`; and `collect`, the lines the pin collects
 *  (its byte budget's measure). */
function rangeWindow(
  count: number,
  first: number,
  last: number | undefined,
  openLines: number
): { start: number; end: number; collect: number } {
  const before = first > 1 ? RANGE_BEFORE : 0;
  const collect = (last === undefined ? openLines : last - first + 1 + RANGE_AFTER) + before;
  const start = first - 1 - before;
  return { start, end: Math.min(count, start + collect), collect };
}

/**
 * Oh My Pi's result for a read of `file` (a path inside `dir`: a written case's checkout copy, or an
 * arm's directory of the cut's own copy) by the path `target` with a line range, its selector taken
 * off: lines `first` to `last`, or from `first` on when `last` is undefined (`:N`, `:N-`), its
 * hashline line naming `shown` and its rows shown as `rows` (read.ts lines 2598-3035 at the pin):
 * the hashline line of the whole file (hashline rows), then the rows `rangeWindow` gives (at most
 * READ_LINES lines from `first` of a range with no end), then `[<n> more lines in file. Use
 * :<next> to continue]` when lines of the file are left after them; `Path '<target>' not found`
 * for a file `dir` lacks; and for a range that starts past the file's end the pin's notice alone
 * (read.ts lines 2750-2765), `Line <first> is beyond end of file (<n> lines total). Use :1 to read
 * from the start, or :<n> to read the last line.`, no error. Where the pin widens a range of a
 * code file to the brackets of the blocks it cuts (`buildLineEntriesWithBlockContext`), this
 * answers the lines alone. Returns why it does not rebuild the read instead: a file of
 * OWN_FORM_FILE, one that is no UTF-8 text, a range of more than READ_LINES lines, a shown line
 * past READ_COLUMNS characters or one that opens a git conflict block (the pin adds a notice of
 * either), and lines past the bytes the pin shows of the range.
 */
export function pathRangeAnswer(
  dir: string,
  read: {
    file: string;
    target: string;
    shown: string;
    rows: PathRows;
    first: number;
    last: number | undefined;
  }
): { readonly text: string; readonly isError: boolean } | string {
  const { full, stats } = armFile(dir, read.file);
  if (stats === undefined) return { text: `Path '${read.target}' not found`, isError: true };
  if (!stats.isFile()) throw new Error(`${full} is not a file: a range is read of a file only`);
  if (OWN_FORM_FILE.test(read.file))
    return `${read.file} is a file a read shows in a form of its own`;
  const bytes = readFileSync(full);
  if (bytes.includes(0) || !isUtf8(bytes)) return `${read.file} is not UTF-8 text`;
  const raw = bytes.toString("utf8");
  const lines = raw.split("\n");
  if (lines.length > 1 && lines.at(-1) === "") lines.pop();
  if (read.first > lines.length) {
    const total = lines.length;
    return {
      text: `Line ${read.first} is beyond end of file (${total} lines total). Use :1 to read from the start, or :${total} to read the last line.`,
      isError: false,
    };
  }
  const wanted = read.last === undefined ? READ_LINES : read.last - read.first + 1;
  if (wanted > READ_LINES) {
    return `its range of ${wanted} lines passes the ${READ_LINES} a read shows at once`;
  }
  const { start, end, collect } = rangeWindow(lines.length, read.first, read.last, READ_LINES);
  const window = lines.slice(start, end);
  const cut = window.findIndex((line) => line.length > READ_COLUMNS);
  if (cut >= 0) {
    return `line ${start + cut + 1} of ${read.file} passes the ${READ_COLUMNS} characters a read shows`;
  }
  const opener = window.findIndex((line) => CONFLICT_OPENER.test(line));
  if (opener >= 0) {
    return `line ${start + opener + 1} of ${read.file} opens a git conflict block, which a read follows with a warning`;
  }
  // The bytes the pin lets the lines it collects take (read.ts lines 2656-2670).
  const prefix = read.rows === "plain" ? 0 : String(start + collect).length + 1;
  const budget = Math.min(
    Math.max(SPILL_BYTES, collect * 512),
    SPILL_BYTES - Math.min(collect * prefix, Math.floor(SPILL_BYTES / 2))
  );
  const spent = window.reduce((sum, line) => sum + Buffer.byteLength(line) + 1, -1);
  if (spent > budget) {
    return `the lines of ${read.file} its range shows come to ${spent} bytes, past the ${budget} a read shows of them`;
  }
  const rows = window.map((line, index) => pathRow(start + index + 1, line, read.rows)).join("\n");
  const header = read.rows === "hashline" ? `[${read.shown}#${fileTag(raw)}]\n` : "";
  const left = lines.length - end;
  const notice = left > 0 ? `\n\n[${left} more lines in file. Use :${end + 1} to continue]` : "";
  return { text: `${header}${rows}${notice}`, isError: false };
}

/** Oh My Pi's text for a `skill://` read of the skill file `text` with a line range, `first` to
 *  `last` (to the end when undefined), the file shown at `shown` (read.ts lines 2508-2517 and
 *  2039-2045, read-format.ts's `buildInMemoryTextResult` at the pin): its `[Skill file: <shown>]`
 *  line, then the lines `rangeWindow` gives (every line from `first` of a range with no end), as a
 *  whole `skill://` read shows them (no row numbers), then `[<n> more lines in file. Use :<next>
 *  to continue]` when a range with an end leaves lines after them; the lines alone where the pin
 *  widens the range to the brackets of the blocks it cuts. Undefined for a range that starts past
 *  the file's end, whose notice is not rebuilt. */
function skillRangeText(
  text: string,
  shown: string,
  range: { readonly first: number; readonly last: number | undefined }
): string | undefined {
  const lines = textLines(text);
  if (range.first > lines.length) return undefined;
  const { start, end } = rangeWindow(lines.length, range.first, range.last, lines.length);
  const left = lines.length - end;
  const notice =
    range.last !== undefined && left > 0
      ? `\n\n[${left} more lines in file. Use :${end + 1} to continue]`
      : "";
  return `${skillFileHeader(shown)}\n${lines.slice(start, end).join("\n")}${notice}`;
}

/** The answer to a new read of `target` from the arm's files, when it is a read of a file of the
 *  arm's copy of the skill by path (cases.ts's `pathReadFile` of `arm.pathCopy`: the cut's own
 *  copy, or the installed directory a case that names none takes for one), whole or of one line
 *  range (`readRange`): Oh My Pi's result (a range as `pathRangeAnswer` gives it), its
 *  hashline line naming the file under the directory the case's reads of the copy show
 *  (`PathCopy.shownDir`) and its rows as they show them, or its not-found result for the path as
 *  read. Undefined for any other read (another selector among them), a read of a directory of
 *  the copy, and a read of a file Oh My Pi shows in a form the arms do not rebuild
 *  (`pathFormFault`, `pathRangeAnswer`'s faults). */
export function pathFile(arm: Arm, target: string): { text: string; isError: boolean } | undefined {
  const copy = arm.pathCopy;
  const ranged = readRange(target);
  if (ranged !== undefined && ranged.range === undefined) return undefined;
  const whole = ranged?.path ?? target;
  const file = pathReadFile(arm, whole);
  if (copy === undefined || file === undefined) return undefined;
  if (arm.skillDir !== undefined && armFile(arm.skillDir, file).stats?.isDirectory()) {
    return undefined;
  }
  const shown = path.join(copy.shownDir, file);
  if (ranged?.range !== undefined) {
    if (arm.skillDir === undefined) return { text: `Path '${whole}' not found`, isError: true };
    const read = { file, target: whole, shown, rows: copy.rows, ...ranged.range };
    const answer = pathRangeAnswer(arm.skillDir, read);
    return typeof answer === "string" ? undefined : answer;
  }
  const read = { file, target, source: `${copy.dir}/${file}`, shown, rows: copy.rows };
  const answer = pathAnswer(arm.skillDir, read);
  return typeof answer === "string" ? undefined : { text: answer.text, isError: answer.isError };
}

/** A hashline line: `[<path>#<tag>]`, the path as Oh My Pi shows it. */
export const HASHLINE_HEADER = /^\[(.+)#[0-9A-F]{4}\]$/;

/** How a read result of a path shows its rows, and the path its hashline line shows; undefined
 *  for one that shows neither: a failed read, a raw one (`:raw` shows no row numbers in any
 *  session), or one whose text no longer shows the rows its `details` name. */
function shownRows(message: SessionMessage): { rows: PathRows; shown?: string } | undefined {
  const details = message.details;
  if (message.isError === true || typeof details !== "object" || details === null) return undefined;
  const prefixes = Reflect.get(details, "rowPrefixes");
  if (Reflect.get(details, "raw") === true || typeof prefixes !== "boolean") return undefined;
  if (!prefixes) return { rows: "plain" };
  const first = (message.content?.[0]?.text ?? "").split("\n", 1)[0] ?? "";
  const header = HASHLINE_HEADER.exec(first);
  if (header?.[1] !== undefined) return { rows: "hashline", shown: header[1] };
  return /^\d+\|/.test(first) ? { rows: "numbered" } : undefined;
}

/** How a new read by path shows its rows in `c`'s session: as the last read by path of its
 *  messages shows them (`shownRows`), else hashline, or plain when the case's recorded tools leave
 *  out the edit tool. */
function pathRowsOf(c: Case, targets: ReadonlyMap<string, string>): PathRows {
  let rows: PathRows | undefined;
  for (const message of c.messages) {
    if (message.role !== "toolResult" || message.toolName !== "read") continue;
    const target = targets.get(message.toolCallId ?? "");
    if (target === undefined || URL_TARGET.test(target)) continue;
    rows = shownRows(message)?.rows ?? rows;
  }
  const edit = c.profile.tools === undefined || c.profile.tools.includes("edit");
  return rows ?? (edit ? "hashline" : "plain");
}

/** The copy of `c`'s skill a read by path reads (PathCopy), or undefined when it has none: the
 *  cut's own copy the case names (`c.pathCopy`), else the directory the case's `skill://` reads
 *  `reads` show the skill installed in (`shownInstalledDir`), whose files a read by path opens as
 *  Oh My Pi's read does; `shownDir` from the first hashline line of a read of a file of the copy
 *  that shows that file's path, else the copy's directory; `rows` as the session shows them
 *  (`pathRowsOf`). */
function pathCopyOf(
  c: Case,
  targets: ReadonlyMap<string, string>,
  rows: PathRows,
  reads: Iterable<{ readonly file: string; readonly header: string | undefined }>
): PathCopy | undefined {
  const installed = shownInstalledDir(reads);
  const place: Case["pathCopy"] =
    c.pathCopy ?? (installed === undefined ? undefined : { dir: installed });
  if (place === undefined) return undefined;
  let shownDir: string | undefined;
  for (const message of c.messages) {
    if (message.role !== "toolResult" || message.toolName !== "read") continue;
    const target = targets.get(message.toolCallId ?? "");
    if (target === undefined) continue;
    const shown = shownRows(message)?.shown;
    const file = pathReadFile({ pathCopy: place }, target);
    if (file === undefined || shown === undefined) continue;
    if (shown === file) shownDir = "";
    else if (shown.endsWith(`/${file}`)) shownDir = shown.slice(0, -(file.length + 1));
    if (shownDir !== undefined) break;
  }
  return {
    dir: place.dir,
    ...(place.cwd === undefined ? {} : { cwd: place.cwd }),
    ...(place.home === undefined ? {} : { home: place.home }),
    shownDir: shownDir ?? place.dir,
    rows,
  };
}

/** The installed directory of each skill of `c`'s profile other than its own that a `skill://`
 *  read of it in `c`'s messages shows: the first `[Skill file: …]` line of such a read, less the
 *  file it read, by the name the profile lists the skill under. */
function skillDirsOf(c: Case, targets: ReadonlyMap<string, string>): Record<string, string> {
  const names = c.profile.skills
    .map((skill) => listedName(skill))
    .filter((name) => name !== c.skill)
    .toSorted((a, b) => b.length - a.length);
  const dirs: Record<string, string> = {};
  for (const message of c.messages) {
    if (message.role !== "toolResult" || message.toolName !== "read") continue;
    const target = targets.get(message.toolCallId ?? "") ?? "";
    for (const name of names) {
      const file = skillReadFile(name, target);
      if (file === undefined) continue;
      const header = skillHeaderPath(message.content?.[0]?.text ?? "");
      if (!Object.hasOwn(dirs, name) && header?.endsWith(`/${file}`)) {
        dirs[name] = header.slice(0, -(file.length + 1));
      }
      break;
    }
  }
  return dirs;
}

/** A skill of an arm's profile other than the case's, as the capture installed it (context.ts's
 *  `installedCopies`): the name the profile lists it under, and the directory of its installed
 *  copy, which a read of it is answered from, the same in every arm. */
export interface InstalledCopy {
  readonly name: string;
  readonly dir: string;
}

/** A new read of a file of a skill other than the case's, answered (`otherSkillRead`): the skill
 *  (the name the profile lists it under, or, for a skill it does not list, the name the
 *  `skill://` URL gives), the file inside its directory, how it was read (`skill` for a
 *  `skill://` URL, `path` for a whole read by path), where the answer came from (`copy`, the
 *  skill's installed copy; `arm`, the arm's directory, for a copy of the case's skill's source
 *  under another name; `absent`, Oh My Pi's unknown-skill result for a skill the arm does not
 *  install), and the answer. */
export interface OtherSkillRead {
  readonly skill: string;
  readonly file: string;
  readonly by: "skill" | "path";
  readonly from: "copy" | "arm" | "absent";
  readonly text: string;
  readonly isError: boolean;
}

/** For a skill of the arm's profile, the arm's directory it is installed from, or undefined when
 *  it is not: the arm's `skillDir` for each copy of the arm's skill's source (cases.ts's
 *  `testedCopy`: the case's skill's, or in `ours` ours'), and undefined for every skill in an arm
 *  with no skill directory (`none`). A copy of another source listed under the case's skill's
 *  name (`caseNameCopy`: in `none` and `ours`; `Arm.relisted`) is never one. context.ts installs
 *  each skill by this rule. */
export function installedFromArm(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills">
): (skill: Pick<ProfileSkill, "repo" | "path">) => string | undefined {
  const { skillDir } = arm;
  if (skillDir === undefined) return () => undefined;
  const tested = testedCopy(arm.profileSkills, arm.skill);
  return (skill) => (tested(skill) ? skillDir : undefined);
}

/** The copy of another source that the arm lists under the case's skill's name (cases.ts's
 *  `testedSource` of that name, `ours.theirs` in `ours`; `Arm.relisted`), or undefined when it
 *  lists none: only `none` and `ours` leave out every copy of the case's skill's source, so only
 *  they can, and `ours` cannot where ours holds that name itself. A `skill://` read of the case's
 *  skill in such an arm reads that copy. */
export function caseNameCopy(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills" | "ours">
): ProfileSkill | undefined {
  const copy = testedSource(arm.profileSkills, arm.ours?.theirs ?? arm.skill);
  return copy === undefined || installedFromArm(arm)(copy) !== undefined ? undefined : copy;
}

/** The file of the arm's skill (`arm.skill`: the case's, or ours in `ours`) that a `skill://` read
 *  of `target` reads from the arm's own files (cases.ts's `skillReadFile`), or undefined when it
 *  reads none: always in `none` where its profile lists a copy of another source under the case's
 *  skill's name (`caseNameCopy`), which a read of that name reads instead. */
export function ownSkillReadFile(
  arm: Pick<Arm, "skill" | "skillDir" | "profileSkills" | "ours">,
  target: string
): string | undefined {
  const copy = caseNameCopy(arm);
  if (copy !== undefined && listedName(copy) === arm.skill) return undefined;
  return skillReadFile(arm.skill, target);
}

/** The directory a read of `skill`, a skill of the arm's profile other than the case's, or the
 *  copy `caseNameCopy` gives, is answered from, and which one it is: the arm's directory for a
 *  copy of the case's skill's source under another name (`installedFromArm`), else its installed
 *  copy in `copies`. Throws naming the skill when `copies` holds no copy of it, or its copy's
 *  directory is gone, so that no read of it is answered as not found. */
function sourceOf(
  arm: Arm,
  copies: readonly InstalledCopy[],
  skill: ProfileSkill
): { dir: string; from: "arm" | "copy" } {
  const armDir = installedFromArm(arm)(skill);
  if (armDir !== undefined) return { dir: armDir, from: "arm" };
  const name = listedName(skill);
  const copy = copies.find((each) => each.name === name);
  if (copy === undefined) {
    throw new Error(
      `the ${arm.name} arm's profile installs ${name}, and the replay holds no installed copy of it (context.ts's installedCopies of its capture's record)`
    );
  }
  if (!statSync(copy.dir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Error(`the installed copy of ${name}, ${copy.dir}, is not a directory`);
  }
  return { dir: copy.dir, from: "copy" };
}

/** The path Oh My Pi's hashline line shows for a read by path that opens `full`: relative to the
 *  session's working directory for a file under it, `~/<rest>` for a file under its home, else
 *  `full` (the cases' recorded reads by path show these three forms); the path as read, made
 *  normal, when the case names no directory that resolves it (`full` undefined). */
export function shownPath(
  target: string,
  full: string | undefined,
  where: { readonly cwd?: string; readonly home?: string }
): string {
  if (full === undefined) return path.posix.normalize(target);
  if (where.cwd !== undefined && full.startsWith(`${where.cwd}/`)) {
    return full.slice(where.cwd.length + 1);
  }
  if (where.home !== undefined && full.startsWith(`${where.home}/`)) {
    return `~${full.slice(where.home.length)}`;
  }
  return full;
}

/** A `skill://` read of `target` (its selector taken off) answered when it reads a file of a skill
 *  of `others`, the profile's skills other than the case's, or the copy `caseNameCopy` gives, or
 *  of a skill the profile does not list (see the file's header), whole or of the line range
 *  `range` (`rangeAnswer`; undefined for a range past the file's end). Oh My Pi's resolver tries
 *  `<name>/<first part of the path>` before `<name>`, so the longest listed name is tried first. */
function otherSkillUrlRead(
  arm: Arm,
  copies: readonly InstalledCopy[],
  others: readonly ProfileSkill[],
  target: string,
  range: { readonly first: number; readonly last: number | undefined } | undefined
): OtherSkillRead | undefined {
  for (const skill of others.toSorted((a, b) => listedName(b).length - listedName(a).length)) {
    const name = listedName(skill);
    const file = skillReadFile(name, target);
    if (file === undefined) continue;
    const { dir, from } = sourceOf(arm, copies, skill);
    if (armFile(dir, file).stats?.isDirectory()) return undefined;
    const shownDir =
      arm.skillDirs !== undefined && Object.hasOwn(arm.skillDirs, name)
        ? arm.skillDirs[name]
        : undefined;
    const shown = path.join(shownDir ?? path.join(path.dirname(arm.installedDir), name), file);
    const answer =
      range === undefined ? fileAnswer(dir, file, shown) : rangeAnswer(dir, file, shown, range);
    return answer === undefined ? undefined : { skill: name, file, by: "skill", from, ...answer };
  }
  const name = /^skill:\/\/([^/]+)/.exec(target)?.[1];
  const file = name === undefined || name.includes(":") ? undefined : skillReadFile(name, target);
  if (name === undefined || file === undefined) return undefined;
  return { skill: name, file, by: "skill", from: "absent", ...unknownSkill(arm, name) };
}

/** A whole read by path of `target` answered when it reads a file of a skill of `others`, the
 *  profile's skills other than the case's, by count.sql's rule: a path that ends
 *  `/<name>/SKILL.md`, or holds `skills/<name>/references/` and more. A name that two of them
 *  share, or that the case's skill has, names none. A file the skill's copy lacks is not
 *  answered: the session's read opened a file on its own disk, which the copy may not hold. */
function otherSkillPathRead(
  arm: Arm,
  copies: readonly InstalledCopy[],
  others: readonly ProfileSkill[],
  target: string
): OtherSkillRead | undefined {
  if (!wholeReadTarget(target)) return undefined;
  const fromArm = installedFromArm(arm);
  const taken = new Set([arm.skill, path.posix.basename(arm.skill)]);
  for (const skill of arm.profileSkills) if (fromArm(skill) !== undefined) taken.add(skill.name);
  const byName = new Map<string, ProfileSkill | null>();
  for (const skill of others) {
    if (!taken.has(skill.name)) byName.set(skill.name, byName.has(skill.name) ? null : skill);
  }
  const parts = path.posix.normalize(target).split("/");
  const at = parts.findLastIndex(
    (part, index) =>
      byName.has(part) &&
      ((index === parts.length - 2 && parts[index + 1] === "SKILL.md") ||
        (parts[index - 1] === "skills" &&
          parts[index + 1] === "references" &&
          index < parts.length - 2))
  );
  const skill = byName.get(parts[at] ?? "");
  if (skill === undefined || skill === null) return undefined;
  const file = parts.slice(at + 1).join("/");
  const { dir, from } = sourceOf(arm, copies, skill);
  if (!armFile(dir, file).stats?.isFile()) return undefined;
  const where = arm.pathCopy ?? {};
  const full = resolvedReadPath(target, where);
  const answer = pathAnswer(dir, {
    file,
    target,
    source: full ?? target,
    shown: shownPath(target, full, where),
    rows: newReadRows(arm),
  });
  if (typeof answer === "string") return undefined;
  return {
    skill: listedName(skill),
    file,
    by: "path",
    from,
    text: answer.text,
    isError: answer.isError,
  };
}

/** The answer to a new read of `target` when it reads a file of a skill other than the case's
 *  (see the file's header): a `skill://` read of a skill the profile lists from the copy
 *  `sourceOf` gives, at its installed directory, whole or of one line range (`readRange`); a
 *  `skill://` read of a skill it does not list with Oh My Pi's unknown-skill result; a whole read
 *  by path (`otherSkillPathRead`) in Oh My Pi's form at the path the session shows. In an arm
 *  whose profile lists a copy of another source under the case's skill's name (`caseNameCopy`:
 *  `none`, or `ours` once their copies are gone), a `skill://` read of the case's skill is a read
 *  of that copy, answered as a read of another skill is. Undefined for any other read: any other
 *  read of the arm's skill, one with another selector, a read by path with any, one of a
 *  directory, and one by path that names no skill of the profile or a file its copy lacks or shows
 *  in a form the arms do not rebuild (`pathFormFault`). Throws when the profile lists the skill
 *  and `copies` holds no copy of it (`sourceOf`). */
export function otherSkillRead(
  arm: Arm,
  copies: readonly InstalledCopy[],
  target: string
): OtherSkillRead | undefined {
  const ranged = readRange(target);
  const whole = ranged?.path ?? target;
  const own = caseNameCopy(arm);
  if (ownSkillReadFile(arm, whole) !== undefined) return undefined;
  const others = arm.profileSkills.filter(
    (skill) => skill === own || listedName(skill) !== arm.skill
  );
  if (whole.startsWith("skill://")) {
    if (ranged !== undefined && ranged.range === undefined) return undefined;
    return otherSkillUrlRead(arm, copies, others, whole, ranged?.range);
  }
  if (URL_TARGET.test(target)) return undefined;
  return otherSkillPathRead(arm, copies, others, target);
}

/** A read result of the case's messages that read `file` of the copy `copy` by the path `target`,
 *  answered from the arm's directory `skillDir` in the form the result shows (`shownRows`; else
 *  the copy's), at its own hashline line's path (else the copy's). Throws naming the case when Oh
 *  My Pi shows the arm's file in a form the arms do not rebuild. */
function pathAnswered(
  c: Case,
  message: SessionMessage,
  arm: { name: ArmName; skillDir: string | undefined },
  read: { file: string; target: string; copy: PathCopy }
): SessionMessage {
  const own = shownRows(message);
  const answer = pathAnswer(arm.skillDir, {
    file: read.file,
    target: read.target,
    source: `${read.copy.dir}/${read.file}`,
    shown: own?.shown ?? path.join(read.copy.shownDir, read.file),
    rows: own?.rows ?? read.copy.rows,
  });
  if (typeof answer === "string") {
    throw new Error(
      `case ${c.id}: the ${arm.name} arm cannot answer the read of ${read.target}: ${answer}, a form the arms do not rebuild`
    );
  }
  return {
    ...message,
    content: [{ type: "text", text: answer.text }],
    details: answer.details,
    isError: answer.isError,
  };
}

/** The directory `dir` as an absolute path, or a throw naming the arm when it is no directory. */
function directory(dir: string, name: ArmName): string {
  if (!statSync(dir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Error(`the ${name} directory ${dir} is not a directory`);
  }
  return path.resolve(dir);
}

/** Leaves out of `file` each line that is `line` (trimmed). Returns how many it left out. */
function removeLine(file: string, line: string): number {
  const text = readFileSync(file, "utf8");
  const lines = text.split("\n");
  const kept = lines.filter((each) => each.trim() !== line.trim());
  if (kept.length < lines.length) writeFileSync(file, kept.join("\n"));
  return lines.length - kept.length;
}

/** Whether `from`, a path of the old directory `old`, is copied into minus: not when it is a link
 *  that names nothing, so a read of it answers not found from the copy as from `old`. Throws when
 *  it is a link to `old` or to a directory that holds it, whose copy would hold itself. */
function copiedIntoMinus(old: string, from: string): boolean {
  if (from === old) return true;
  if (!existsSync(from)) return false;
  const back = path.relative(realpathSync(from), realpathSync(old));
  if (back !== ".." && !back.startsWith(`..${path.sep}`) && !path.isAbsolute(back)) {
    throw new Error(`${from} names a directory that holds ${old}, so minus cannot copy it`);
  }
  return true;
}

/** The `minus` directory: a copy of `old` under `scratch`, less each line of its baseFile that is
 * the case's line (trimmed), or less each line that is `plantedLine` in each file of the copy that
 * holds it. Each symbolic link of `old` is copied as the file or directory it names, so an edit of
 * the copy never writes through a link into `old` or elsewhere; a link that names nothing is left
 * out (`copiedIntoMinus`). Throws when no line is left out. */
function minusDirectory(c: Case, dirs: ArmDirs, scratch: string): string {
  const old = directory(dirs.old, "old");
  if (dirs.plantedLine?.trim() === "") throw new Error("the planted line is blank");
  const dir = path.resolve(scratch, "minus");
  rmSync(dir, { recursive: true, force: true });
  cpSync(old, dir, {
    recursive: true,
    dereference: true,
    filter: (from) => copiedIntoMinus(old, from),
  });
  const { plantedLine } = dirs;
  const removed =
    plantedLine === undefined
      ? removeLine(path.join(dir, c.baseFile), c.line.text)
      : readdirSync(dir, { recursive: true, encoding: "utf8" })
          .map((file) => path.join(dir, file))
          .filter((file) => statSync(file).isFile())
          .reduce((sum, file) => sum + removeLine(file, plantedLine), 0);
  if (removed === 0) {
    const what =
      plantedLine === undefined
        ? `its line is not in ${c.baseFile}`
        : `the planted line is in no file of ${old}`;
    throw new Error(`case ${c.id}: the minus arm would equal old: ${what}`);
  }
  return dir;
}

/** A read of a file of the case's skill in its history: the file (a path inside the skill), the
 *  target its call read, the path its result's header shows, and its call's id. */
interface HistoryRead {
  readonly file: string;
  readonly target: string;
  readonly header: string | undefined;
  readonly id: string;
}

/** A whole read by path of a file of the cut's own copy in the case's history: the file (a path
 *  inside the skill), the target its call read, its call's id, and the copy as the session read
 *  it (PathCopy). */
interface PathRead {
  readonly file: string;
  readonly target: string;
  readonly id: string;
  readonly copy: PathCopy;
}

/** `messages` less each read result at an index of `removed` and each call of those reads, an
 *  assistant message left with no text and no tool call going too: what `none` gives of the
 *  history. */
function withoutReads(
  messages: readonly SessionMessage[],
  removed: ReadonlyMap<number, { readonly id: string }>
): SessionMessage[] {
  const ids = new Set([...removed.values()].map((read) => read.id));
  return messages.flatMap((message, index): SessionMessage[] => {
    if (removed.has(index)) return [];
    const content = message.content;
    if (message.role !== "assistant" || content === undefined) return [structuredClone(message)];
    const kept = content.filter((part) => !(part.type === "toolCall" && ids.has(part.id ?? "")));
    if (kept.length === content.length) return [structuredClone(message)];
    if (!kept.some((part) => part.type === "text" || part.type === "toolCall")) return [];
    return [structuredClone({ ...message, content: kept })];
  });
}

/** The arm of ours of case `c` (the module's comment says what it holds), from `ours`, our
 *  directory and name; `history` holds the case's `skill://` reads of its skill's files and its
 *  whole reads by path of the cut's own copy, each by index, and the copy a read by path reads
 *  (`pathCopyOf`: that copy, or the installed directory); `installedDir` is the reads' installed
 *  directory. */
function oursArm(
  c: Case,
  ours: { readonly dir: string; readonly name: string },
  history: {
    readonly reads: ReadonlyMap<number, HistoryRead>;
    readonly pathReads: ReadonlyMap<number, PathRead>;
    readonly pathCopy: PathCopy | undefined;
  },
  installedDir: string
): Arm {
  const { reads, pathReads, pathCopy } = history;
  const skillDir = directory(ours.dir, "ours");
  // Theirs goes as `none` leaves it out: each copy of its source. Ours takes the place of the copy
  // listed under their name, or of a skill of another source listed under ours, never beside it.
  // Then each name is listed as Oh My Pi lists it, as in `none` (`relistedSkills`): ours, bare in
  // `skills/`, keeps its name from a namespaced copy that shares it.
  const tested = testedCopy(c.profile.skills, c.skill);
  const replacedOwn = c.profile.skills.some(
    (skill) => !tested(skill) && listedName(skill) === ours.name
  );
  const { skills, relisted } = relistedSkills(
    c.profile.skills.flatMap((skill) => {
      if (!tested(skill)) return [skill];
      if (replacedOwn || listedName(skill) !== c.skill) return [];
      return [{ repo: skill.repo, commit: skill.commit, path: skill.path, name: ours.name }];
    })
  );
  const arm: Arm = {
    name: "ours",
    skill: ours.name,
    skillDir,
    installedDir: path.join(path.dirname(installedDir), ours.name),
    messages: [],
    profileSkills: skills,
    ...(relisted.length === 0 ? {} : { relisted }),
    ...(pathCopy === undefined ? {} : { pathCopy }),
  };
  // Each history read of theirs, `skill://` or by path, whose file ours lacks, in the history's
  // order: it goes with its call, as `none` removes a `skill://` read.
  const lacking = new Map<number, { readonly file: string; readonly id: string }>(
    [...reads, ...pathReads]
      .filter(([, read]) => !armFile(skillDir, read.file).stats?.isFile())
      .toSorted(([a], [b]) => a - b)
  );
  // Each kept read's call, by its id, reads our skill's URL at the same path inside the skill.
  const theirs = `skill://${c.skill}`;
  const targets = new Map(
    [...reads]
      .filter(([index]) => !lacking.has(index))
      .map(([, read]) => [read.id, `skill://${ours.name}${read.target.slice(theirs.length)}`])
  );
  const messages = c.messages.map((message, index): SessionMessage => {
    // A read by path of a file ours has keeps its call and its path, answered from our file.
    const byPath = pathReads.get(index);
    if (byPath !== undefined && !lacking.has(index)) return pathAnswered(c, message, arm, byPath);
    const read = reads.get(index);
    if (read !== undefined && !lacking.has(index)) {
      const target = targets.get(read.id) ?? read.target;
      const shown = path.join(arm.installedDir, read.file);
      return answered(message, arm, { file: read.file, target, shown });
    }
    const content = message.content;
    if (message.role !== "assistant" || content === undefined) return message;
    const renamed = content.map((part) => {
      const target = part.type === "toolCall" ? targets.get(part.id ?? "") : undefined;
      if (target === undefined) return part;
      return { ...part, arguments: { ...Object(part.arguments), path: target } };
    });
    return { ...message, content: renamed };
  });
  return {
    ...arm,
    // withoutReads copies every message it keeps, so no message here shares the case's objects.
    ...withInjections(c, withoutReads(messages, lacking), skillDir, ours.name),
    dispatch: dispatchOf(c),
    ours: {
      theirs: c.skill,
      replacedOwn,
      removedReads: [...lacking.values()].map((read) => read.file),
    },
  };
}

/** A table row and a table's separator row, as Oh My Pi's `format` finds them. */
const TABLE_ROW = /^\|.*\|$/;
const TABLE_SEP = /^\|[-:\s|]+\|$/;

/** A table separator row with each cell `---`, `:---`, `---:` or `:---:`, as `format` writes it. */
function compactSeparator(line: string): string {
  const cells = line
    .split("|")
    .filter((cell) => cell.trim() !== "")
    .map((cell) => {
      const trimmed = cell.trim();
      const left = trimmed.startsWith(":");
      const right = trimmed.endsWith(":");
      return `${left ? ":" : ""}---${right ? ":" : ""}`;
    });
  return `|${cells.join("|")}|`;
}

/**
 * `content` as Oh My Pi's `format` leaves a rendered prompt (packages/utils/src/prompt.ts lines
 * 192-308 at the pin, with its defaults: no ASCII-symbol or RFC 2119 rewrite): each line less its
 * trailing whitespace; a line that opens a ``` or ~~~ fence, and each line inside one, kept as it
 * is; elsewhere a run of two or more blank lines dropped whole, a single blank line dropped where
 * it opens the text, the blank lines before a closing tag line (`</name>`) dropped, and a table
 * row's cells less their padding (a separator row's each `---`, `:---`, `---:` or `:---:`); then
 * the text's last blank lines dropped.
 */
function formatPrompt(content: string): string {
  const lines = content.split("\n");
  const result: string[] = [];
  const dropBlanks = () => {
    while (result.length > 0 && result.at(-1) === "") result.pop();
  };
  const blank = (line: string | undefined) => line !== undefined && !/\S/.test(line);
  let fenced = false;
  for (let i = 0; i < lines.length; i++) {
    let line = (lines[i] ?? "").trimEnd();
    // The indent: spaces and tabs, or any leading whitespace where a character above ASCII
    // follows them, as `format` measures it.
    let s = 0;
    let first = line.charCodeAt(0);
    while (first === 32 || first === 9) first = line.charCodeAt(++s);
    if (first >= 128) {
      s = line.length - line.trimStart().length;
      first = line.charCodeAt(s);
    }
    if (
      (first === 96 || first === 126) &&
      (line.startsWith("```", s) || line.startsWith("~~~", s))
    ) {
      fenced = !fenced;
      result.push(line);
      continue;
    }
    if (fenced) {
      result.push(line);
      continue;
    }
    const rest = line.slice(s);
    // A closing tag line, `</name>` of `a`-`z`, `-` and `_`.
    const closing = first === 60 && /^<\/[a-z_-]+>$/.test(rest);
    if (first === 124 && TABLE_SEP.test(rest)) {
      line = `${line.slice(0, s)}${compactSeparator(rest)}`;
    } else if (first === 124 && TABLE_ROW.test(rest)) {
      line = `${line.slice(0, s)}${rest
        .split("|")
        .map((cell) => cell.trim())
        .join("|")}`;
    }
    if (s >= line.length) {
      const next = lines[i + 1];
      if (next === undefined || blank(next)) {
        dropBlanks();
        while (blank(lines[i + 1])) i++;
        continue;
      }
      if (result.length === 0 || result.at(-1) === "") continue;
    }
    if (closing) dropBlanks();
    result.push(line);
  }
  dropBlanks();
  return result.join("\n");
}

/** The text of Oh My Pi's autoloaded block of the SKILL.md in `skillDir`, naming `file` as its
 *  path: prompts/skills/autoload.md rendered with no arguments (`<body>`, a blank line, `---`, a
 *  blank line, `Skill: <file>`), put through `formatPrompt` and trimmed. The file is read as Oh My
 *  Pi reads it (common.ts's `ompText`: a leading byte order mark dropped), and its body as
 *  `loadSkillPromptBody` reads it: `parseFrontmatter` with `repair: false`, its body trimmed
 *  (packages/utils/src/frontmatter.ts lines 148-163 at the pin), which is common.ts's
 *  `splitFrontmatter` with no `repair`, so no leading HTML comment is dropped. */
export function autoloadedText(skillDir: string, file: string): string {
  const text = ompText(readFileSync(path.join(skillDir, "SKILL.md")));
  const body = splitFrontmatter(text, false).body.trim();
  return formatPrompt(`${body}\n\n---\n\nSkill: ${file}\n`).trim();
}

/** `details.name` and `details.path` of a message, where they are strings. */
function detailsOf(message: SessionMessage): { name?: string; path?: string } {
  const { details } = message;
  if (typeof details !== "object" || details === null) return {};
  const name: unknown = Reflect.get(details, "name");
  const file: unknown = Reflect.get(details, "path");
  return {
    ...(typeof name === "string" ? { name } : {}),
    ...(typeof file === "string" ? { path: file } : {}),
  };
}

/** Whether `message` is a `skill-prompt` message of `skill`, in any form. */
function isSkillPrompt(message: SessionMessage, skill: string): boolean {
  return (
    message.role === "custom" &&
    message.customType === SKILL_PROMPT &&
    detailsOf(message).name === skill
  );
}

/** The SKILL.md path an autoloaded block of `skill` names, when `message` is one in the form Oh My
 *  Pi writes it (see the file's header): a `skill-prompt` message of the skill the user did not
 *  invoke, of one text part that ends `Skill: <its details.path>` after a blank line, `---` and a
 *  blank line. Else undefined. */
function autoloadedPath(message: SessionMessage, skill: string): string | undefined {
  if (!isSkillPrompt(message, skill) || message.attribution === "user") return undefined;
  const file = detailsOf(message).path;
  const [part, ...more] = message.content ?? [];
  if (file === undefined || file === "" || more.length > 0 || part?.type !== "text") {
    return undefined;
  }
  return part.text?.endsWith(`\n\n---\n\nSkill: ${file}`) === true ? file : undefined;
}

/** Where the Envoy plugin puts its block among `messages`: after the leading compaction
 *  summaries (pi-envoy's `withDispatchFirst`). */
function blockPlace(messages: readonly SessionMessage[]): number {
  const at = messages.findIndex((message) => message.role !== "compactionSummary");
  return at === -1 ? messages.length : at;
}

/** Whether `message` opens with the plugin's marker: a user or custom message whose first part's
 *  text does, as the plugin finds its own block (pi-envoy's `leadingText`). */
function opensWithMarker(message: SessionMessage | undefined): boolean {
  return (
    (message?.role === "user" || message?.role === "custom") &&
    message.content?.[0]?.text?.startsWith(DISPATCH_FIRST_MARKER) === true
  );
}

/** Whether the plugin of PLUGIN_INJECTED puts the case's skill into its session's requests: the
 *  case's skill is the one it reads, listed under that bare name, and the profile installs it.
 *  load.ts asks it of a load test's tested skill too. */
export function pluginInjects(c: Pick<Case, "skill" | "profile">): boolean {
  return (
    c.skill === PLUGIN_INJECTED.skill &&
    c.profile.plugins.some((plugin) => plugin.name === PLUGIN_INJECTED.plugin)
  );
}

/** A `dispatch` command at a command's place (skill-scenarios/transcript.ts's AT_COMMAND), and
 *  its subcommand. A `bash` call's command is read with its continued lines joined and its
 *  here-document bodies, data for the command that opens each, left out (`withoutBodies`). */
const DISPATCH_COMMAND = new RegExp(String.raw`${AT_COMMAND}dispatch[ \t]+([a-z][a-z-]*)`, "m");

/** What `message` shows of Dispatch, as the end of a sentence that starts "its message <index>",
 *  or undefined when it shows none: a user, custom or developer message whose text holds the
 *  dispatch-first block (its marker, which opens the Envoy plugin's block and Claude Code's
 *  session-start hook output of it alike: packages/claude-envoy/hooks/session-hook.ts), or an
 *  assistant message that calls a Dispatch tool by name (`dispatch_<command>`), writes a Dispatch
 *  tool device (`xd://dispatch_<command>`) or runs a `dispatch` command in a `bash` call. A tool
 *  result is no evidence: a read of a file that quotes the marker holds it. */
function dispatchShown(message: SessionMessage): string | undefined {
  if (message.role === "user" || message.role === "custom" || message.role === "developer") {
    return message.content?.some((part) => part.text?.includes(DISPATCH_FIRST_MARKER) === true)
      ? `holds the dispatch-first block (${DISPATCH_FIRST_MARKER}), which a host puts only into a session with Dispatch`
      : undefined;
  }
  if (message.role !== "assistant") return undefined;
  for (const part of message.content ?? []) {
    if (part.type !== "toolCall") continue;
    const name = part.name ?? "";
    const field = (key: string): string => {
      const value: unknown =
        typeof part.arguments === "object" && part.arguments !== null
          ? Reflect.get(part.arguments, key)
          : undefined;
      return typeof value === "string" ? value : "";
    };
    if (/^dispatch_[a-z_]+$/.test(name)) return `calls the Dispatch tool ${name}`;
    if (name === "write" && field("path").startsWith("xd://dispatch_")) {
      return `writes the Dispatch tool device ${field("path")}`;
    }
    const command =
      name === "bash"
        ? DISPATCH_COMMAND.exec(withoutBodies(field("command")).code)?.[1]
        : undefined;
    if (command !== undefined) return `runs \`dispatch ${command}\` in a bash call`;
  }
  return undefined;
}

/** The Legion store (`legion:<project>`) a case's session is of: from its context
 *  (`legion:<project>:<role>`), or, for a subagent (`subagent:<agent>`), from the evidence item
 *  `subagent <agent>` build.ts writes, whose text names the store's context; undefined for a dev
 *  box's session, or a subagent whose evidence names no Legion store. */
export function legionStore(c: Pick<Case, "context" | "evidence">): string | undefined {
  const own = /^(legion:[^:\s]+):/.exec(c.context)?.[1];
  if (own !== undefined) return own;
  const agent = /^subagent:(.+)$/.exec(c.context)?.[1];
  if (agent === undefined) return undefined;
  const item = c.evidence?.find((each) => each.item === `subagent ${agent}`);
  return /\bthe store's context is (legion:[\w-]+)/.exec(item?.source ?? "")?.[1];
}

/** Whether `c`'s session had Dispatch, and why (DispatchAnswer): a Legion role's session, or a
 *  subagent of one (`legionStore`), had it; so did a session one of whose messages shows it
 *  (`dispatchShown`, the first such message); any other, a dev box's, is taken to have had it. */
export function dispatchOf(c: Pick<Case, "context" | "messages" | "evidence">): DispatchAnswer {
  const agent = /^subagent:(.+)$/.exec(c.context)?.[1];
  const store = legionStore(c);
  if (store !== undefined) {
    return {
      from: "legion-role",
      why:
        agent === undefined
          ? `its context ${c.context} is a Legion role's session, and a Legion role works its issue through Dispatch`
          : `it is the session of the subagent ${agent} of a Legion role's session: its evidence item "subagent ${agent}" says the store's context is ${store}`,
    };
  }
  for (const [index, message] of c.messages.entries()) {
    const shown = dispatchShown(message);
    if (shown !== undefined) return { from: "transcript", why: `its message ${index} ${shown}` };
  }
  const whose =
    agent === undefined
      ? `its context is ${c.context}`
      : `it is the session of the subagent ${agent}, whose evidence names no Legion store`;
  return {
    from: "assumed",
    why: `${whose}, and none of its messages holds the dispatch-first block or a Dispatch call, so the runner takes it that the session had Dispatch`,
  };
}

/** `messages` with the plugin's block (see the file's header) of the skill in `skillDir` at its
 *  place, less any block there already, or with none when `skillDir` is undefined; and where the
 *  block went (null for none). */
export function withPluginBlock(
  messages: readonly SessionMessage[],
  skillDir: string | undefined
): { messages: SessionMessage[]; at: number | null } {
  const at = blockPlace(messages);
  const kept =
    messages[at]?.role === "user" && opensWithMarker(messages[at])
      ? messages.toSpliced(at, 1)
      : [...messages];
  if (skillDir === undefined) return { messages: kept, at: null };
  const text = readDispatchFirstContext(path.join(skillDir, "SKILL.md"));
  const block: SessionMessage = { role: "user", content: [{ type: "text", text }], timestamp: 0 };
  return { messages: kept.toSpliced(at, 0, block), at };
}

/** One reason the arms of a case cannot each give the session their own text of its skill
 *  (`injectionFaults`): what kind of block it is, and the sentence that says so. */
export interface InjectionBlockFault {
  readonly kind: "user-invoked" | "other-shape" | "plugin-block-misplaced";
  readonly text: string;
}

/** Each reason the arms of `c` cannot each give the session their own text of its skill, none
 *  when they can, with its kind: each `skill-prompt` message of the skill that is not an
 *  autoloaded block in the form Oh My Pi writes one, one the user invoked with `/skill:`
 *  (`user-invoked`) or one with arguments or another shape (`other-shape`), and, where the plugin
 *  puts the skill into the requests, each message that opens with its marker away from where it
 *  puts its block (`plugin-block-misplaced`). */
export function injectionFaults(
  c: Pick<Case, "skill" | "profile" | "messages">
): InjectionBlockFault[] {
  const faults: InjectionBlockFault[] = [];
  for (const [index, message] of c.messages.entries()) {
    if (isSkillPrompt(message, c.skill) && autoloadedPath(message, c.skill) === undefined) {
      const invoked = message.attribution === "user";
      faults.push({
        kind: invoked ? "user-invoked" : "other-shape",
        text: `its message ${index} is a ${SKILL_PROMPT} message of ${c.skill}${invoked ? ", one the user invoked," : ""} not in the form of Oh My Pi's autoloaded block (one text part that ends "Skill: <its details.path>")`,
      });
    }
  }
  if (pluginInjects(c)) {
    const at = blockPlace(c.messages);
    for (const [index, message] of c.messages.entries()) {
      if (index === at && message.role === "user") continue;
      if (opensWithMarker(message)) {
        faults.push({
          kind: "plugin-block-misplaced",
          text: `its message ${index} opens with ${DISPATCH_FIRST_MARKER} away from where the ${PLUGIN_INJECTED.plugin} plugin puts its block (message ${at}, after the leading compaction summaries)`,
        });
      }
    }
  }
  return faults;
}

/** Why the arms of `c` cannot each give the session their own text of its skill, or undefined
 *  when they can: the text of each of its `injectionFaults`, joined by "; ". */
export function injectionFault(
  c: Pick<Case, "skill" | "profile" | "messages">
): string | undefined {
  const faults = injectionFaults(c);
  return faults.length === 0 ? undefined : faults.map((fault) => fault.text).join("; ");
}

/** `messages`, an arm's, with each block of `c`'s skill that reaches the session beside its
 *  listing line and its reads given the text of the skill in `skillDir`, or left out when it is
 *  undefined (`none`): each autoloaded block rebuilt under its recorded path, and the plugin's
 *  block put in where the plugin puts it into the case's session. With `ours` (our skill's name,
 *  in the arm of ours), an autoloaded block is ours: it names our SKILL.md at the recorded path
 *  with our name as its skill directory's last part, its details our name and that path. */
function withInjections(
  c: Case,
  messages: readonly SessionMessage[],
  skillDir: string | undefined,
  ours?: string
): { messages: SessionMessage[]; injections: Injection[] } {
  const injections: Injection[] = [];
  // Each autoloaded block the arm holds, by its message, with the path it names.
  const rebuilt = new Map<SessionMessage, string>();
  let out: SessionMessage[] = [];
  for (const message of messages) {
    const recorded = autoloadedPath(message, c.skill);
    if (recorded === undefined) out.push(message);
    else if (skillDir === undefined) injections.push({ by: "autoload", path: recorded, at: null });
    else {
      const file =
        ours === undefined
          ? recorded
          : path.join(path.dirname(path.dirname(recorded)), ours, path.basename(recorded));
      const block = {
        ...message,
        ...(ours === undefined
          ? {}
          : { details: { ...Object(message.details), name: ours, path: file } }),
        content: [{ type: "text", text: autoloadedText(skillDir, file) }],
      };
      rebuilt.set(block, file);
      out.push(block);
    }
  }
  if (pluginInjects(c)) {
    const placed = withPluginBlock(out, skillDir);
    out = placed.messages;
    injections.push({
      by: "plugin",
      plugin: PLUGIN_INJECTED.plugin,
      name: PLUGIN_INJECTED.skill,
      at: placed.at,
    });
  }
  for (const [index, message] of out.entries()) {
    const file = rebuilt.get(message);
    if (file !== undefined) injections.push({ by: "autoload", path: file, at: index });
  }
  return { messages: out, injections };
}

/** The directory a read's result header `header` shows its skill installed in: the header less
 *  `/<file>`, `file` being the path the read read inside the skill; undefined when there is no
 *  header, or it does not end so. */
export function installedDirOf(header: string | undefined, file: string): string | undefined {
  return header?.endsWith(`/${file}`) ? header.slice(0, -(file.length + 1)) : undefined;
}

/** The directory skill reads `reads` show their skill installed in: the one the first of them, in
 *  order, whose result header shows one shows (`installedDirOf`), or undefined when none does. */
function shownInstalledDir(
  reads: Iterable<{ readonly file: string; readonly header: string | undefined }>
): string | undefined {
  for (const { file, header } of reads) {
    const dir = installedDirOf(header, file);
    if (dir !== undefined) return dir;
  }
  return undefined;
}

/** The directory the arms show skill `skill` installed in, from its reads `reads` in order: the
 *  first whose result header shows one (`shownInstalledDir`), else `/skills/<skill>`. */
export function installedDirIn(
  skill: string,
  reads: Iterable<{ readonly file: string; readonly header: string | undefined }>
): string {
  return shownInstalledDir(reads) ?? path.join("/skills", skill);
}

/** `skills`, the profile of `none` or `ours` once each copy of the case's skill's source is gone
 *  (and, in `ours`, ours placed), as Oh My Pi lists them, and, for each copy that lost its
 *  namespace, the name `skills` lists it under (see the file's header). Oh My Pi reads `skills/`
 *  first, then each `namespaced/<namespace>/skills` in the order of each namespace's first copy
 *  in `skills` (context.ts's `namespacedDirs`), and lists the first copy of a name it finds under
 *  the bare name. So for each name that no copy holds bare and some copy holds under a namespace,
 *  the copy whose namespace comes first in that order loses its namespace; every other copy keeps
 *  it. */
function relistedSkills(skills: readonly ProfileSkill[]): {
  skills: ProfileSkill[];
  relisted: string[];
} {
  // The names some copy holds bare, and the namespaces in the order Oh My Pi reads their
  // directories.
  const bare = new Set<string>();
  const order = new Set<string>();
  for (const skill of skills) {
    if (skill.namespace === undefined) bare.add(skill.name);
    else order.add(skill.namespace);
  }
  // By name, the first copy of each name no copy holds bare that Oh My Pi finds, reading each
  // namespace's directory in that order.
  const first = new Map<string, ProfileSkill>();
  for (const namespace of order) {
    for (const skill of skills) {
      if (skill.namespace !== namespace || bare.has(skill.name) || first.has(skill.name)) continue;
      first.set(skill.name, skill);
    }
  }
  return {
    skills: skills.map((skill) => {
      if (first.get(skill.name) !== skill) return structuredClone(skill);
      const { namespace: _, ...listedBare } = skill;
      return structuredClone(listedBare);
    }),
    relisted: [...first.values()].map((skill) => listedName(skill)),
  };
}

/** The arm `name` of case `c`, from the skill directories `dirs`, with whether its session had
 * Dispatch (`dispatchOf`). `minus` writes its copy of the old directory to `<scratch>/minus`,
 * replacing one there. Throws naming the case when a skillReads index is not a read of a file of
 * the skill (cases.ts's `skillReadFile` or `pathReadFile`), when Oh My Pi shows an arm's file read
 * by path in a form the arms do not rebuild (`pathFormFault`), or when `dirs` lacks the arm's
 * directory, and InjectionRefused when the case holds a block of its skill the arms cannot each
 * give their text (`injectionFault`). */
export function buildArm(c: Case, name: ArmName, dirs: ArmDirs, scratch: string): Arm {
  const fault = injectionFault(c);
  if (fault !== undefined) throw new InjectionRefused(`case ${c.id}: ${fault}`);
  const targets = readTargets(c.messages);
  const pathRows = pathRowsOf(c, targets);
  // What every arm of the case shares to answer a new read of another skill (otherSkillRead).
  const shared = { pathRows, skillDirs: skillDirsOf(c, targets) };
  // The case's `skill://` reads, and its reads of the cut's own copy by path, by index.
  const reads = new Map<number, HistoryRead>();
  const copyReads = new Map<number, Omit<PathRead, "copy">>();
  for (const index of c.skillReads.toSorted((a, b) => a - b)) {
    const message = c.messages[index];
    const target = targets.get(message?.toolCallId ?? "") ?? "";
    const result = message?.role === "toolResult";
    const file = result ? skillReadFile(c.skill, target) : undefined;
    const byPath = result && file === undefined ? pathReadFile(c, target) : undefined;
    if (file !== undefined) {
      const header = skillHeaderPath(message?.content?.[0]?.text ?? "");
      reads.set(index, { file, target, header, id: message?.toolCallId ?? "" });
    } else if (byPath !== undefined) {
      copyReads.set(index, { file: byPath, target, id: message?.toolCallId ?? "" });
    } else {
      throw new Error(`case ${c.id}: skillReads index ${index} is not a read of a skill file`);
    }
  }
  // The installed directory: the first header's path less the file it read.
  const installedDir = installedDirIn(c.skill, reads.values());
  // The copy a read by path reads: the cut's own copy the case names, which each of `copyReads`
  // reads, else the installed directory the headers show.
  const pathCopy = pathCopyOf(c, targets, pathRows, reads.values());
  const pathReads = new Map<number, PathRead>();
  for (const [index, read] of copyReads) {
    if (pathCopy !== undefined) pathReads.set(index, { ...read, copy: pathCopy });
  }

  if (name === "ours") {
    if (dirs.ours === undefined) throw new Error(`case ${c.id}: the ours arm needs --ours`);
    const ours = oursArm(c, dirs.ours, { reads, pathReads, pathCopy }, installedDir);
    return { ...ours, ...shared };
  }
  if (name === "none") {
    // Each read by path of the copy keeps its call, its result Oh My Pi's not-found one; each
    // `skill://` read goes with its call.
    const messages = withoutReads(
      c.messages.map((message, index) => {
        const byPath = pathReads.get(index);
        if (byPath === undefined) return message;
        return pathAnswered(c, message, { name, skillDir: undefined }, byPath);
      }),
      reads
    );
    const tested = testedCopy(c.profile.skills, c.skill);
    const { skills, relisted } = relistedSkills(c.profile.skills.filter((skill) => !tested(skill)));
    return {
      name,
      skill: c.skill,
      skillDir: undefined,
      installedDir,
      ...withInjections(c, messages, undefined),
      profileSkills: skills,
      ...(relisted.length === 0 ? {} : { relisted }),
      dispatch: dispatchOf(c),
      ...(pathCopy === undefined ? {} : { pathCopy }),
      ...shared,
    };
  }

  const given = name === "new" ? dirs.new : dirs.old;
  if (given === undefined) throw new Error(`case ${c.id}: the new arm needs --new`);
  const skillDir = name === "minus" ? minusDirectory(c, dirs, scratch) : directory(given, name);
  const arm: Arm = {
    name,
    skill: c.skill,
    skillDir,
    installedDir,
    messages: [],
    profileSkills: structuredClone(c.profile.skills),
    ...(pathCopy === undefined ? {} : { pathCopy }),
    ...shared,
  };
  const messages = c.messages.map((message, index) => {
    const byPath = pathReads.get(index);
    if (byPath !== undefined) return structuredClone(pathAnswered(c, message, arm, byPath));
    const read = reads.get(index);
    if (read === undefined) return structuredClone(message);
    // The real result's own header path, else the installed directory's: never the arm's.
    const shown = read.header ?? path.join(installedDir, read.file);
    return structuredClone(answered(message, arm, { ...read, shown }));
  });
  return { ...arm, ...withInjections(c, messages, skillDir), dispatch: dispatchOf(c) };
}
