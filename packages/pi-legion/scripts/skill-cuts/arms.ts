/**
 * The five arms of a case: `old` (the skill's base text, `--old`), `new` (the
 * cut text, `--new`), `none` (no skill), `minus` (a copy of the old directory less every line
 * of the case's baseFile that is the case's line, trimmed, or less every line of any of its files
 * that is `--planted-line`, trimmed, when one is given: a rule written twice is still the rule, so
 * each copy of it goes) and `twin` (the old directory again, built as `old` is: the false-loss
 * control's second copy of the old arm, with its own capture and runs). Only the skill's directory
 * differs between arms: the messages are the case's own but for the text of the skill's files in
 * its read results, and the profile is the case's own but for the skill, which `none` leaves out.
 *
 * In each arm but `none`, each read result of a file of the skill (the case's `skillReads`) is
 * answered from the arm's file the way the pinned Oh My Pi 18.18.6 answers a `skill://` read: one
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
 * `skillFile(arm, file)` answers one new read the same way, at the arm's `installedDir`, and is
 * what replay.ts answers a reply's read with; cases.ts's `skillReadFile(arm.skill, target)` says
 * which file a read reads.
 */
import { cpSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  readTargets,
  type SessionMessage,
  skillFileHeader,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import { type Case, type ProfileSkill, skillReadFile, textLines } from "./cases";
import type { Arm as ScoreArm } from "./score";

/** The name of an arm, as run records carry it. */
export type ArmName = ScoreArm;

/** One arm of a case. */
export interface Arm {
  readonly name: ArmName;
  /** The case's skill, as `skill://<skill>` names it. */
  readonly skill: string;
  /** The arm's directory of the skill, absolute, or undefined for `none`. The model never sees
   *  it. */
  readonly skillDir: string | undefined;
  /** The skill's directory as the model sees it, the same in every arm of a case: the installed
   * directory the case's first `[Skill file: …]` line of the skill shows, or `/skills/<skill>`
   * when the case has none. */
  readonly installedDir: string;
  /** The case's messages, as this arm gives them. */
  readonly messages: SessionMessage[];
  /** The skills the arm's profile installs: the case's own, less the case's skill for `none`. The
   * case's skill is installed from `skillDir`. */
  readonly profileSkills: ProfileSkill[];
}

/** The directories the arms are built from: each is the skill's own directory, SKILL.md at its
 * top. */
export interface ArmDirs {
  readonly old: string;
  readonly new: string;
  /** The line `minus` leaves out in place of the case's line. */
  readonly plantedLine?: string;
}

/** The answer to a read of `file` (a path inside the skill's directory) from the arm's files,
 * shown at `shown`: the header and the text, or the not-found text with `isError`. */
function answer(arm: Arm, file: string, shown: string): { text: string; isError: boolean } {
  if (arm.skillDir === undefined) {
    const names = arm.profileSkills.map((skill) => skill.name);
    const available = names.length > 0 ? names.join(", ") : "none";
    return { text: `Unknown skill: ${arm.skill}\nAvailable: ${available}`, isError: true };
  }
  const full = path.resolve(arm.skillDir, file);
  const inside = path.relative(arm.skillDir, full);
  if (inside === ".." || inside.startsWith(`..${path.sep}`) || path.isAbsolute(inside)) {
    throw new Error(`${file} is not a path inside the skill directory ${arm.skillDir}`);
  }
  const stats = statSync(full, { throwIfNoEntry: false });
  if (stats === undefined) return { text: `File not found: ${shown}`, isError: true };
  if (!stats.isFile()) throw new Error(`${full} is not a file: an arm answers reads of files only`);
  const text = readFileSync(full, "utf8").replace(/\n$/, "");
  return { text: `${skillFileHeader(shown)}\n${text}`, isError: false };
}

/** The answer to a new read of `file` (a path inside the skill's directory) from the arm's files,
 * at the arm's `installedDir`: the header and the text, or the not-found text with `isError`. */
export function skillFile(arm: Arm, file: string): { text: string; isError: boolean } {
  return answer(arm, file, path.join(arm.installedDir, file));
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

/** The `minus` directory: a copy of `old` under `scratch`, less each line of its baseFile that is
 * the case's line (trimmed), or less each line that is `plantedLine` in each file of the copy that
 * holds it. Each symbolic link of `old` is copied as the file or directory it names, so an edit of
 * the copy never writes through a link into `old` or elsewhere. Throws when no line is left out. */
function minusDirectory(c: Case, dirs: ArmDirs, scratch: string): string {
  const old = directory(dirs.old, "old");
  if (dirs.plantedLine?.trim() === "") throw new Error("the planted line is blank");
  const dir = path.resolve(scratch, "minus");
  rmSync(dir, { recursive: true, force: true });
  cpSync(old, dir, { recursive: true, dereference: true });
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

/** The arm `name` of case `c`, from the skill directories `dirs`. `minus` writes its copy of the
 * old directory to `<scratch>/minus`, replacing one there. Throws naming the case when a
 * skillReads index is not a read of a `skill://<skill>` file. */
export function buildArm(c: Case, name: ArmName, dirs: ArmDirs, scratch: string): Arm {
  const targets = readTargets(c.messages);
  const reads = new Map(
    c.skillReads
      .toSorted((a, b) => a - b)
      .map((index) => {
        const message = c.messages[index];
        const target = targets.get(message?.toolCallId ?? "") ?? "";
        const file = message?.role === "toolResult" ? skillReadFile(c.skill, target) : undefined;
        if (file === undefined) {
          throw new Error(`case ${c.id}: skillReads index ${index} is not a read of a skill file`);
        }
        const header = skillHeaderPath(message?.content?.[0]?.text ?? "");
        return [index, { file, target, header, id: message?.toolCallId ?? "" }];
      })
  );
  // The installed directory: the first header's path less the file it read.
  let installedDir = path.join("/skills", c.skill);
  for (const { file, header } of reads.values()) {
    if (header?.endsWith(`/${file}`)) {
      installedDir = header.slice(0, -(file.length + 1));
      break;
    }
  }

  if (name === "none") {
    const ids = new Set([...reads.values()].map((read) => read.id));
    const messages = c.messages.flatMap((message, index): SessionMessage[] => {
      if (reads.has(index)) return [];
      const content = message.content;
      if (message.role !== "assistant" || content === undefined) return [structuredClone(message)];
      const kept = content.filter((part) => !(part.type === "toolCall" && ids.has(part.id ?? "")));
      if (kept.length === content.length) return [structuredClone(message)];
      if (!kept.some((part) => part.type === "text" || part.type === "toolCall")) return [];
      return [structuredClone({ ...message, content: kept })];
    });
    return {
      name,
      skill: c.skill,
      skillDir: undefined,
      installedDir,
      messages,
      profileSkills: structuredClone(c.profile.skills.filter((skill) => skill.name !== c.skill)),
    };
  }

  const skillDir =
    name === "minus"
      ? minusDirectory(c, dirs, scratch)
      : directory(dirs[name === "twin" ? "old" : name], name);
  const arm: Arm = {
    name,
    skill: c.skill,
    skillDir,
    installedDir,
    messages: [],
    profileSkills: structuredClone(c.profile.skills),
  };
  const messages = c.messages.map((message, index) => {
    const read = reads.get(index);
    if (read === undefined) return structuredClone(message);
    // The real result's own header path, else the installed directory's: never the arm's.
    const shown = read.header ?? path.join(installedDir, read.file);
    return structuredClone(answered(message, arm, { ...read, shown }));
  });
  return { ...arm, messages };
}
