/**
 * The case file of the behaviour test: one JSON object per line, each one step of a real Oh My Pi
 * session that a line of one skill's text governs. Its writer is `skill-cuts build-cases`
 * (build.ts), and `behaviour` (cli.ts) and build.ts read it through `loadCases`. A case file holds
 * the cases of one skill: the runner is run once per skill, with that skill's old and new
 * directories.
 *
 * A read of a skill file is a `read` tool call of `skill://<skill>` (its SKILL.md) or
 * `skill://<skill>/<path>`, as `skillReadFile` reads one, or a whole read (no read selector) of a
 * file of the cut's own copy of the skill by its path on disk, absolute, relative to the session's
 * working directory or under `~`, when the case names that copy (`pathCopy`), as `pathReadFile`
 * reads one; `caseReadFile` says which file either reads. Every read result of the case's
 * messages that answers one is in `skillReads`, and the arms answer it with their own text
 * (arms.ts). A read of another copy, or one with a selector, stays as the session had it.
 */
import { createHash } from "node:crypto";
import { closeSync, openSync, readFileSync, readSync, statSync } from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { readCalls, readTargets, SessionMessage } from "../skill-scenarios/transcript";
import { schemaFaults } from "./common";
import { CaseKind } from "./score";

/** A file of a repository at a commit: the repository (`owner/name`), the commit, and the path
 * inside it; `.` for the repository's root (a skill at the root, build.ts). */
const RepoPath = {
  repo: z.string().regex(/^[^/\s]+\/[^/\s]+$/),
  commit: z.string().min(1),
  path: z.string().min(1),
};
/** An installed skill directory of a case's profile, and the name it is installed under. A
 *  `namespace` marks a second copy of a name whose text differs from the first's, which Oh My Pi
 *  lists as `<namespace>/<name>` (extensibility/skills.ts, lines 164-223); context.ts installs it
 *  in a skills directory of its own under that namespace. */
export const ProfileSkill = z.strictObject({
  ...RepoPath,
  name: z.string().min(1),
  namespace: z
    .string()
    .regex(/^[\p{L}\p{N}_-]+$/u, "is not one token of letters, digits, _ and -")
    .optional(),
});
export type ProfileSkill = z.infer<typeof ProfileSkill>;

/** The name Oh My Pi lists a profile skill under: `<namespace>/<name>` for a namespaced copy, else
 *  its name. Every check of a profile's names reads this one. */
export function listedName(skill: Pick<ProfileSkill, "name" | "namespace">): string {
  return skill.namespace === undefined ? skill.name : `${skill.namespace}/${skill.name}`;
}

/** The copy of `skills` listed under the tested skill's name `name`, or undefined when none is. */
export function testedSource(
  skills: readonly ProfileSkill[],
  name: string
): ProfileSkill | undefined {
  return skills.find((skill) => listedName(skill) === name);
}

/** Which of `skills` are copies of the source of the tested skill `name`: the repository and path
 *  (at any commit, under any namespace) of the copy listed under `name` itself (`testedSource`).
 *  An arm gives each of them its own text (`none`, and the arm of ours, which installs ours in
 *  their place, leave each out); a copy of another source that shares the name stays as the
 *  profile has it in every arm but those two, each of which lists one such copy under the bare
 *  name where no copy left holds that name bare (arms.ts's `Arm.relisted`; the arm of ours not
 *  where ours holds that name itself). With no copy listed under `name`, none is. */
export function testedCopy(
  skills: readonly ProfileSkill[],
  name: string
): (skill: Pick<ProfileSkill, "repo" | "path">) => boolean {
  const source = testedSource(skills, name);
  return (skill) =>
    source !== undefined && skill.repo === source.repo && skill.path === source.path;
}

/** A directory inside a repository's checkout: relative, `/`-separated, with no empty, `.` or
 * `..` part. */
const CheckoutDir = z
  .string()
  .min(1)
  .refine(
    (dir) => dir.split("/").every((part) => part !== "" && part !== "." && part !== ".."),
    "is not a directory inside the repository's checkout"
  );

/** What the capture installs for a case, the same in every arm but the case's skill (context.ts):
 * the installed skills, the plugins the session's context installed at its
 * start, the context files (the session repository's, at their paths in it; a Legion project's
 * instructions file; and, marked `user: true`, a dev box's user-level file, Claude Code's
 * `CLAUDE.md` in its config directory), `sessionRepository`, the repository the session's working
 * directory is a checkout of and the commit its project skills and context files were read at
 * (build.ts; absent when the build found none), `workingDirectory`, the session's working
 * directory inside the session repository's checkout (absent: its root), and, for a Legion
 * context, the role prompt that goes to `--append-system-prompt`. A subagent's
 * session records its own system prompt and tools (its `session_init` line), so its case carries
 * them: `systemPrompt`, the recorded blocks the capture sends in place of a rebuilt prompt, with
 * only the case skill's line of their skill listing changed per arm (and, in a `none` or an arm
 * of ours that lists a copy of another source under its bare name, that copy's
 * `<namespace>/<name>` line left out: arms.ts's `Arm.relisted`), and `tools`, the recorded tool
 * names the capture restricts Oh My Pi's tools to. */
export const Profile = z.strictObject({
  skills: z.array(ProfileSkill),
  plugins: z.array(z.strictObject({ ...RepoPath, name: z.string().min(1) })),
  contextFiles: z.array(z.strictObject({ ...RepoPath, user: z.literal(true).optional() })),
  sessionRepository: z.strictObject({ repo: RepoPath.repo, commit: RepoPath.commit }).optional(),
  workingDirectory: CheckoutDir.optional(),
  appendSystemPrompt: z.string().min(1).optional(),
  systemPrompt: z.array(z.string()).min(1).optional(),
  tools: z.array(z.string().min(1)).min(1).optional(),
});
export type Profile = z.infer<typeof Profile>;

/** The thinking levels `omp --thinking` takes. */
export const Thinking = z.enum(["off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"]);

/** A path inside a skill directory: relative, with no `..` part. */
const InsidePath = z
  .string()
  .min(1)
  .refine(
    (file) => !path.isAbsolute(file) && !file.split(/[\\/]/).includes(".."),
    "is not a path inside the skill directory"
  );

/** An absolute directory as a session reads files under it: `/`, then parts that are neither
 * empty, `.` nor `..` (a CheckoutDir), with no `/` at its end. */
const AbsoluteDir = z
  .string()
  .refine(
    (dir) => dir.startsWith("/") && CheckoutDir.safeParse(dir.slice(1)).success,
    "is not an absolute directory with no / at its end"
  );

/** One case. `line` is the line of the cut's base text that the step exercises: its text,
 * `lineNumber` (from 1) and `lineCount` (the lines of `baseFile`; a final newline ends the last
 * line, it does not start one). `criterion` says what the line requires at this step, when a reply
 * passes and when it fails; it is written before any run. `model` and `thinking` are the
 * transcript's last `model_change` and `thinking_level_change` before the step. `context` is
 * `devbox`, `legion:<project>:<role>` (`architect-root` for a tree's root architect), or
 * `subagent:<agent>` for a subagent's session, whose profile carries its recorded system prompt
 * and tools. `messages` are the session's Oh My Pi messages up to the step, `skillReads` the
 * indices in `messages` of the read results of this skill's files (`caseReadFile`), and
 * `realStep` the real agent's next assistant message. `pathCopy`, when the case builder wrote it,
 * is the cut's own copy of the skill as the session read it by path: `dir`, its absolute
 * directory, with no `/` at its end, and what resolves a read's path to it as Oh My Pi's read
 * resolves one (`resolvedReadPath`): `cwd`, the session's working directory, for a relative
 * path, and `home`, the home directory it ran under, for a path under `~`; each absent when the
 * builder did not know it. A case whose session read the skill by `skill://` only has none.
 * `violatingStep` is an assistant message the case reader wrote before any run: a plausible next
 * step that breaks the line and otherwise looks like a valid one (the same goal, the same kind of
 * tool calls); the judge must fail it (judge.ts, `judgeControls`). `source`
 * names the store and the SHA-256 of the transcript's path (`pathHash`), never the path, and the
 * case's kind (`caseKind`): `session` (one step of that session) or `written` (a written task in
 * that session's context, `build-cases --written`); a case with no kind is a session case, so a
 * case file written before cases had a kind loads as it was. `checkout`, on a written case whose
 * context session's working directory is in a checkout of a repository the build found (build.ts's
 * `sessionRepository`), is that checkout: `repo`, the repository, `commit`, the commit its project
 * was read at (the profile's `sessionRepository`), `root`, the checkout's directory on the
 * session's machine, `cwd`, the session's working directory, inside `root`, `jj`, whether the
 * context session ran jj (a `bash` call of its transcript whose command holds the word `jj`,
 * checkout.ts's `messagesRunJj`), so its checkout was a jj workspace (absent on a case written
 * before it was recorded), and `bashGate`, the bash gate of the dotfiles' experiments extension
 * that session ran (build.ts's `sessionBashGate`): `leading`, its arm of `bash_gate_leading` in
 * its last `experiments` entry before the skill's first read, and `versions`, the versions of the
 * gate (checkout.ts's BASH_GATE_COMMITS) whose text form matches every refusal of the gate it
 * got before that read (`gateRefusalVersions`; absent where it got none); `bashGate` is absent
 * on a case written before it was recorded, or whose session records no such arm, and then no
 * `bash` call of the case is answered (cli.ts's `caseShell`); and `knives`, what the knives
 * plugin added to the context session's tool results for its checkout's repository (build.ts's
 * `sessionKnives`): `blocks`, its blocks of that repository in the first addition that held any
 * (the notice naming a root that holds the working directory, then the guidance of the same
 * repository), byte for byte, null where it holds none; `touched`, whether a call of the session
 * touched the repository as knives reads its paths (checkout.ts's `knivesTouches`), so knives
 * decided its blocks then; absent on a case written before it was recorded. A run adds the
 * blocks to its first answer that touches the repository (checkout.ts's `KnivesRepository`). A
 * run of the case answers a reply's read-only calls from a copy of `repo` at `commit`
 * (replay.ts), its paths mapped between `root` and the copy, a colocated jj repository where `jj`
 * (cli.ts). A case with none gets no such answers, and a written case's run with none gets no
 * verdict on a reply whose calls all read, one of them other than a skill read (replay.ts's
 * UnansweredRead, its why cli.ts's NO_CHECKOUT): the run is made again.
 * `secretsRemoved` counts what the secret scan took out of the case before it left the pod, and
 * `sessionDigest` is the session's last `system-prompt-digest` before the step, or null.
 * `evidence`, when the case builder wrote it, says where each part of the case came from that the
 * transcript does not show by itself (the context's role, each skill, plugin and context file of
 * the profile), one `{item, source}` per part, so a reader can check it. */
export const Case = z.strictObject({
  id: z.string().min(1),
  cut: z.string().min(1),
  skill: z.string().min(1),
  baseFile: InsidePath,
  line: z.strictObject({
    text: z
      .string()
      .refine((text) => text.trim() !== "" && !text.includes("\n"), "is not one non-blank line"),
    lineNumber: z.int().positive(),
    lineCount: z.int().positive(),
  }),
  criterion: z.strictObject({ pass: z.string().min(1), fail: z.string().min(1) }),
  model: z.string().min(1),
  thinking: Thinking,
  context: z.string().regex(/^(legion:[^:\s]+:[^:\s]+|devbox|subagent:[^:\s]+)$/),
  profile: Profile,
  messages: z.array(SessionMessage),
  skillReads: z.array(z.int().nonnegative()),
  pathCopy: z
    .strictObject({ dir: AbsoluteDir, cwd: AbsoluteDir.optional(), home: AbsoluteDir.optional() })
    .optional(),
  realStep: SessionMessage.refine(
    (message) => message.role === "assistant",
    "is not an assistant message"
  ),
  violatingStep: SessionMessage.refine(
    (message) => message.role === "assistant",
    "is not an assistant message"
  ).refine(
    (message) =>
      (message.content ?? []).some(
        (block) =>
          (block.type === "text" && (block.text ?? "").trim() !== "") || block.type === "toolCall"
      ),
    "holds no text and no tool call"
  ),
  source: z.strictObject({
    store: z.string().min(1),
    pathHash: z.string().regex(/^[0-9a-f]{64}$/),
    kind: CaseKind.optional(),
  }),
  checkout: z
    .strictObject({
      repo: RepoPath.repo,
      commit: RepoPath.commit,
      root: AbsoluteDir,
      cwd: AbsoluteDir,
      jj: z.boolean().optional(),
      bashGate: z
        .strictObject({
          leading: z.boolean(),
          versions: z.array(z.string().min(1)).optional(),
        })
        .optional(),
      knives: z
        .strictObject({ blocks: z.string().min(1).nullable(), touched: z.boolean() })
        .optional(),
    })
    .refine(
      (checkout) => insideRoot(checkout.root, checkout.cwd) !== undefined,
      "names a working directory outside the checkout's root"
    )
    .optional(),
  sessionStart: z.iso.datetime({ offset: true }),
  secretsRemoved: z.int().nonnegative(),
  sessionDigest: z.string().min(1).nullable(),
  evidence: z
    .array(z.strictObject({ item: z.string().min(1), source: z.string().min(1) }))
    .optional(),
});
export type Case = z.infer<typeof Case>;

/** The `source.pathHash` of a transcript path. */
export function pathHash(transcriptPath: string): string {
  return createHash("sha256").update(transcriptPath).digest("hex");
}

/** Whether case `c` is a session case or a written one: its `source.kind`, a case with none read
 *  as a session case. */
export function caseKind(c: Pick<Case, "source">): CaseKind {
  return c.source.kind ?? "session";
}

/** The file of skill `skill` that a `read` of `target` reads, as a path inside the skill
 * directory (`SKILL.md` for `skill://<skill>` itself), or undefined when `target` is no
 * `skill://` URL of that skill, carries a selector (`:`), or leaves the skill directory. */
export function skillReadFile(skill: string, target: string): string | undefined {
  const url = `skill://${skill}`;
  if (target !== url && !target.startsWith(`${url}/`)) return undefined;
  let file: string;
  try {
    file = decodeURIComponent(target.slice(url.length + 1));
  } catch {
    return undefined;
  }
  if (file === "") return "SKILL.md";
  return file.includes(":") || !InsidePath.safeParse(file).success ? undefined : file;
}

/** The files of `files` (paths inside the skill's directory) that the `read` calls of `step` read,
 *  each once, in order, as the arms tell a read of a file of the skill of case `c`: a read of
 *  `skill://<skill>/<path>`, or a whole read by path of the case's own copy (`caseReadFile`, which
 *  resolves a relative or `~` path as Oh My Pi's read does); for a case that names no copy, a read
 *  of a path that ends `<installedDir>/<path>`, the directory the skill is installed at, too. The
 *  rule of the behaviour report's line on such steps (cli.ts) and of build-cases'
 *  `--drop-navigation` (build.ts). */
export function stepSkillFiles(
  step: SessionMessage,
  c: Pick<Case, "skill" | "pathCopy">,
  installedDir: string,
  files: readonly string[]
): string[] {
  const read = readCalls([{ message: step }]).flatMap(({ target }) => {
    const file =
      caseReadFile(c, target) ??
      (c.pathCopy === undefined
        ? files.find((each) => target.endsWith(`${installedDir}/${each}`))
        : undefined);
    return file !== undefined && files.includes(file) ? [file] : [];
  });
  return [...new Set(read)];
}

/** A `read` target with a scheme (`skill://`, `https://`): no path on disk. */
export const URL_TARGET = /^[a-z][a-z0-9+.-]*:\/\//i;
/** A read selector after a read's last `/` (`:raw`, `:1-40`). */
export const READ_SELECTOR = /:[^/]*$/;

/** Whether a read of `target` reads a whole file: no read selector after its last `/`
 *  (READ_SELECTOR) and no `/` at its end (a directory). */
export function wholeReadTarget(target: string): boolean {
  return !target.endsWith("/") && !READ_SELECTOR.test(target);
}

/** The absolute path the pinned Oh My Pi's `read` opens for the path `target` (its read selector
 *  already taken off), resolved with the working directory `cwd` and the home directory `home`, or
 *  undefined when it needs one of them that is absent, or `target` is a URL. As `expandPath` and
 *  `resolveToCwd` do (packages/coding-agent/src/tools/path-utils.ts lines 109-123, 323-325 and
 *  337-343 at the tag v18.18.6-sami.20261119-162325 of cybermonklabs/oh-my-pi; read.ts line 2194
 *  calls them through `resolveReadPathAsync`, lines 1248-1286, whose other spellings it tries only
 *  for a file that is not there): a `:` before a path's start is taken off (line 118), then an `@`
 *  before `/`, `~` or `~/` (lines 75-95); a leading `~` is the home directory, `~/<rest>` the home
 *  and `/<rest>`, `~<rest>` the two joined (packages/utils/src/path.ts lines 59-70); an absolute
 *  path is kept as it is, and any other is resolved against the working directory
 *  (`path.resolve`). */
export function resolvedReadPath(
  target: string,
  where: { readonly cwd?: string; readonly home?: string }
): string | undefined {
  if (URL_TARGET.test(target)) return undefined;
  let read = /^:(?=[/~]|\.\.?\/)/.test(target) ? target.slice(1) : target;
  if (read.startsWith("@/") || read === "@~" || read.startsWith("@~/")) read = read.slice(1);
  if (read.startsWith("~")) {
    if (where.home === undefined) return undefined;
    read =
      read === "~"
        ? where.home
        : read.startsWith("~/")
          ? `${where.home}${read.slice(1)}`
          : path.posix.join(where.home, read.slice(1));
  }
  if (path.posix.isAbsolute(read)) return read;
  return where.cwd === undefined ? undefined : path.posix.resolve(where.cwd, read);
}

/** The path inside the directory `root` of the absolute path `full`, "" for `root` itself, or
 *  undefined when it is outside `root` (both as given: neither is normalized or resolved). */
export function insideRoot(root: string, full: string): string | undefined {
  if (full === root) return "";
  return full.startsWith(`${root}/`) ? full.slice(root.length + 1) : undefined;
}

/** The file of the case's own copy of its skill (`pathCopy`) that a `read` of the path `target`
 * reads whole, as a path inside the copy (`SKILL.md` for `<dir>/SKILL.md`), or undefined when the
 * case names no copy, `target` carries a read selector (a `:` after its last `/`, as `:1-180` or
 * `:raw`) or names a directory (a `/` at its end), or the path Oh My Pi's read opens for it
 * (`resolvedReadPath`, with the copy's `cwd` and `home`) is not under `<dir>/`, leaves the copy,
 * or needs a `cwd` or `home` the copy does not name. */
export function pathReadFile(
  c: {
    readonly pathCopy?: { readonly dir: string; readonly cwd?: string; readonly home?: string };
  },
  target: string
): string | undefined {
  if (c.pathCopy === undefined) return undefined;
  if (!wholeReadTarget(target)) return undefined;
  const full = resolvedReadPath(target, c.pathCopy);
  if (full === undefined || !full.startsWith(`${c.pathCopy.dir}/`)) return undefined;
  const file = full.slice(c.pathCopy.dir.length + 1);
  return InsidePath.safeParse(file).success ? file : undefined;
}

/** The file of `c`'s skill that a `read` of `target` reads, as a path inside the skill's
 * directory: a `skill://` read's (`skillReadFile`) or a whole read's of the case's own copy by
 * path (`pathReadFile`); undefined for any other read. */
function caseReadFile(c: Pick<Case, "skill" | "pathCopy">, target: string): string | undefined {
  return skillReadFile(c.skill, target) ?? pathReadFile(c, target);
}

/** The lines of a file's text: a final newline ends the last line, it does not start one. */
export function textLines(text: string): string[] {
  return text.replace(/\n$/, "").split("\n");
}

/** Why `c` cannot run against the base text in `baseDir`, whose file `c.baseFile` holds `lines`
 *  (undefined when it cannot be read), or undefined when it can. */
function refusal(
  c: Case,
  baseDir: string,
  lines: readonly string[] | undefined
): string | undefined {
  const file = path.join(baseDir, c.baseFile);
  if (lines === undefined) return `its baseFile ${c.baseFile} is not a file of ${baseDir}`;
  const wanted = c.line.text.trim();
  if (!lines.some((line) => line.trim() === wanted)) {
    return `its line is not a whole line of ${file}: ${JSON.stringify(c.line.text)}`;
  }
  if (lines.length !== c.line.lineCount) {
    return `its line.lineCount is ${c.line.lineCount}, and ${file} has ${lines.length} lines`;
  }
  if (lines[c.line.lineNumber - 1]?.trim() !== wanted) {
    return `line ${c.line.lineNumber} of ${file} is not its line`;
  }
  if (!c.profile.skills.some((skill) => listedName(skill) === c.skill)) {
    return `its profile does not install the skill ${c.skill}`;
  }
  const targets = readTargets(c.messages);
  const files =
    c.pathCopy === undefined
      ? `a skill://${c.skill} file`
      : `a skill://${c.skill} file or a whole read of a file of ${c.pathCopy.dir}`;
  for (const index of c.skillReads) {
    const message = c.messages[index];
    if (message?.role !== "toolResult" || message.toolName !== "read") {
      return `skillReads index ${index} is not a read result`;
    }
    const target = targets.get(message.toolCallId ?? "");
    if (target === undefined || caseReadFile(c, target) === undefined) {
      return `skillReads index ${index} answers ${target === undefined ? "no read call before it" : `a read of ${target}`}, not a read of ${files}`;
    }
  }
  // A read of the skill's files left out of skillReads would keep the base text in every arm.
  for (const [index, message] of c.messages.entries()) {
    if (message.role !== "toolResult" || c.skillReads.includes(index)) continue;
    const target = targets.get(message.toolCallId ?? "");
    if (target !== undefined && caseReadFile(c, target) !== undefined) {
      return `the read result at index ${index} (${target}) is not in skillReads`;
    }
  }
  return undefined;
}

/** The lines of `file` as `String.split("\n")` gives them from its whole text, the one after the
 *  last newline included, read a chunk at a time so that the whole text is never held. */
function* fileLines(file: string): Generator<string> {
  const fd = openSync(file, "r");
  try {
    const chunk = Buffer.alloc(1 << 20);
    let start: Buffer[] = [];
    for (;;) {
      const size = readSync(fd, chunk, 0, chunk.length, null);
      if (size === 0) break;
      const read = chunk.subarray(0, size);
      let from = 0;
      for (let end = read.indexOf(10); end !== -1; end = read.indexOf(10, from)) {
        start.push(read.subarray(from, end));
        yield Buffer.concat(start).toString("utf8");
        start = [];
        from = end + 1;
      }
      // A line the next chunk ends: its start is copied, since the next read reuses `chunk`.
      if (from < size) start.push(Buffer.from(read.subarray(from)));
    }
    yield Buffer.concat(start).toString("utf8");
  } finally {
    closeSync(fd);
  }
}

/** The cases of `file`, each checked against the base text of its skill, the directory `baseDir`
 * (the skill's own directory, SKILL.md at its top). Throws one error that names each case it
 * refuses and why: a line that is no case, a missing realStep or violatingStep, a line.text that
 * is not a whole line (trimmed) of `baseDir/<baseFile>` or is not at its lineNumber, a skillReads
 * index that is not a read result of a file of the skill (`caseReadFile`: a `skill://<skill>`
 * file, or a file of the case's own copy read whole by path), a read result of one that
 * skillReads leaves out, a skill its profile does not install, an id given twice, or cases of
 * more than one skill. */
export function loadCases(file: string, baseDir: string): Case[] {
  if (!statSync(baseDir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Error(`the base directory ${baseDir} is not a directory`);
  }
  const cases: Case[] = [];
  const refusals: string[] = [];
  // Each base file's lines, read once for all the cases that name it; undefined when unreadable.
  const baseLines = new Map<string, string[] | undefined>();
  const linesOf = (baseFile: string): string[] | undefined => {
    if (!baseLines.has(baseFile)) {
      let lines: string[] | undefined;
      try {
        lines = textLines(readFileSync(path.join(baseDir, baseFile), "utf8"));
      } catch {
        lines = undefined;
      }
      baseLines.set(baseFile, lines);
    }
    return baseLines.get(baseFile);
  };
  let number = 0;
  for (const text of fileLines(file)) {
    number += 1;
    if (text.trim() === "") continue;
    const where = `${file}:${number}`;
    let raw: unknown;
    try {
      raw = JSON.parse(text);
    } catch (error) {
      refusals.push(`${where} is not JSON: ${messageFor(error)}`);
      continue;
    }
    const id =
      typeof raw === "object" && raw !== null && "id" in raw && typeof raw.id === "string"
        ? raw.id
        : undefined;
    const name = id === undefined ? `the case at ${where}` : `case ${id} (${where})`;
    if (typeof raw === "object" && raw !== null && !("realStep" in raw)) {
      refusals.push(`${name} has no realStep (the real agent's next assistant message)`);
      continue;
    }
    if (typeof raw === "object" && raw !== null && !("violatingStep" in raw)) {
      refusals.push(
        `${name} has no violatingStep (a step that breaks the line, written before any run): build its case again`
      );
      continue;
    }
    const parsed = Case.safeParse(raw);
    if (!parsed.success) {
      refusals.push(`${name} is not a case: ${schemaFaults(parsed.error, "the case")}`);
      continue;
    }
    const why = refusal(parsed.data, baseDir, linesOf(parsed.data.baseFile));
    if (why !== undefined) {
      refusals.push(`${name}: ${why}`);
      continue;
    }
    if (cases.some((c) => c.id === parsed.data.id)) {
      refusals.push(`${name}: its id is given twice`);
      continue;
    }
    cases.push(parsed.data);
  }
  const other = cases.find((c) => c.skill !== cases[0]?.skill);
  if (other !== undefined) {
    refusals.push(
      `${file} holds cases of more than one skill: ${cases[0]?.skill} and ${other.skill} (one skill per case file)`
    );
  }
  if (refusals.length > 0) throw new Error(`refused cases:\n${refusals.join("\n")}`);
  return cases;
}
