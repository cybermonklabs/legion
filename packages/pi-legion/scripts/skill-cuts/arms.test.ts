import { beforeAll, expect, test } from "bun:test";
import { lstatSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { readDispatchFirstContext } from "@legion/envoy-client/dispatch-first";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  SessionEntry,
  type SessionMessage,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import {
  type Arm,
  type ArmName,
  autoloadedText,
  buildArm,
  caseNameCopy,
  type DispatchAnswer,
  dispatchOf,
  InjectionRefused,
  type InstalledCopy,
  injectionFault,
  injectionFaults,
  otherSkillRead,
  pathAnswer,
  pathFile,
  pathFormFault,
  skillFile,
  snapshotTag,
  unknownSkill,
} from "./arms";
import { type Case, pathHash } from "./cases";
import { pinNatives } from "./test-natives";

// The arms of a case, on F1 (testdata/F1): an Oh My Pi 18.18.6 session of a scratch profile with
// the made-up skills alpha and beta, captured with `HOME=<scratch> omp -p
// --session-dir <scratch>/sessions --model anthropic/claude-sonnet-5-5 "<prompt>"`
// (testdata/CAPTURE.md). Its messages are: 0 the user's prompt, 1 a read of skill://alpha, 2 its
// result, 3 a read of skill://alpha/references/x.md, 4 its result, 5 a read of the missing
// skill://gamma, 6 its not-found result, 7 a `task` call (the step here), and the rest.

const F1 = path.join(
  import.meta.dir,
  "testdata",
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
const scratch = temporaryDirectory("skill-cuts-arms", { real: true });
/** The `test` of the rows whose reads need the pinned Oh My Pi's native addon (a window that leaves
 *  lines of a file out, for its block context, or a code file the pin may summarize): skipped, or
 *  under GITHUB_ACTIONS failing, where the runner finds none (test-natives.ts). */
const natives = pinNatives("arms.test.ts");
const LINE = "2. Write each step's result on one line.";
const PLANTED = "Run the tests where needed.";

/** F1's messages, in order. */
function f1Messages(): SessionMessage[] {
  return readFileSync(F1, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => SessionEntry.parse(JSON.parse(line)))
    .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));
}
const messages = f1Messages();

/** The one text of a message. */
function textOf(message: SessionMessage | undefined): string {
  return message?.content?.[0]?.text ?? "";
}

/** A message's `details`, the tool's own record, as an object. */
function detailsOf(message: SessionMessage | undefined): Record<string, unknown> {
  const details = message?.details;
  return typeof details === "object" && details !== null ? { ...details } : {};
}

/** The file F1's read result at `index` read, as it was on disk: its text less the header, with
 * the newline the read drops. */
function f1File(index: number): string {
  return `${stripSkillHeader(textOf(messages[index]))}\n`;
}

/** A skill directory under the scratch directory holding `files` (path inside it to text). */
function skillDir(name: string, files: Record<string, string>): string {
  const dir = path.join(scratch, name);
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
    writeFileSync(path.join(dir, file), text);
  }
  return dir;
}

/** The old text: alpha's files as F1 read them. The new text: a SKILL.md with the line
 * rewritten, and no references/x.md; the whole new text also rewrites references/x.md. Written
 * once the scratch directory is made. */
const oldDir = path.join(scratch, "old");
const newSkill = f1File(2).replace(LINE, "2. Put each step's result on a line of its own.");
const newX = f1File(4).replace("counted from one", "numbered from one");
const newDir = path.join(scratch, "new");
const wholeNewDir = path.join(scratch, "whole-new");
beforeAll(() => {
  skillDir("old", { "SKILL.md": f1File(2), "references/x.md": f1File(4) });
  skillDir("new", { "SKILL.md": newSkill });
  skillDir("whole-new", { "SKILL.md": newSkill, "references/x.md": newX });
});
/** Where F1's profile installed alpha, as its first `[Skill file: …]` line shows it. */
const INSTALLED = path.dirname(skillHeaderPath(textOf(messages[2])) ?? "");

/** The case of F1's `task` step, which line 9 of alpha's SKILL.md governs. */
const f1Case: Case = {
  id: "c1",
  cut: "acme-skills-1",
  skill: "alpha",
  baseFile: "SKILL.md",
  line: { text: LINE, lineNumber: 9, lineCount: 10 },
  criterion: {
    pass: "The reply writes the result of step 4 on one line.",
    fail: "The reply spreads a step's result over more than one line.",
  },
  model: "anthropic/claude-sonnet-5-5",
  thinking: "off",
  context: "devbox",
  profile: {
    skills: [
      { repo: "acme/skills", commit: "0123456789abcdef", path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: "0123456789abcdef", path: "skills/beta", name: "beta" },
    ],
    plugins: [],
    contextFiles: [],
  },
  messages: messages.slice(0, 7),
  skillReads: [2, 4],
  realStep: messages[7] ?? {},
  violatingStep: { role: "assistant", content: [{ type: "text", text: "Step 4." }] },
  source: { store: "store_a", pathHash: pathHash("/sessions/alpha.jsonl") },
  sessionStart: "2026-11-20T01:59:41.780Z",
  secretsRemoved: 0,
  sessionDigest: "46874d907e557998",
};
const original = structuredClone(f1Case);
/** The ids of the case's reads of alpha's files. */
const readIds = f1Case.skillReads.map((index) => messages[index]?.toolCallId);

function arm(name: ArmName, plantedLine?: string): Arm {
  const dirs = { old: oldDir, new: newDir, ...(plantedLine === undefined ? {} : { plantedLine }) };
  return buildArm(f1Case, name, dirs, path.join(scratch, `scratch-${name}-${plantedLine ?? ""}`));
}

/** A read result of the skill as F1 holds one, at F1's installed path: the header, then the
 * file's text less its newline. */
function readResult(file: string, text: string): string {
  return `[Skill file: ${path.join(INSTALLED, file)}]\n${text.replace(/\n$/, "")}`;
}

test("every read result of the skill's files is replaced by the arm's file with its header", () => {
  const old = arm("old");
  expect(old.skillDir).toBe(oldDir);
  expect(old.installedDir).toBe(INSTALLED);
  // Built from the files F1 read, the old arm gives F1's own messages, byte for byte.
  expect(JSON.stringify(old.messages)).toBe(JSON.stringify(f1Case.messages));
  const fresh = arm("new");
  expect(textOf(fresh.messages[2])).toBe(readResult("SKILL.md", newSkill));
  expect(fresh.messages[2]?.details).toEqual({
    ...detailsOf(messages[2]),
    displayContent: {
      text: newSkill.replace(/\n$/, ""),
      startLine: 1,
      lineNumbers: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
    },
  });
  // A new read, as replay asks one, is answered at the same installed path.
  expect(skillFile(fresh, "SKILL.md")).toEqual({
    text: readResult("SKILL.md", newSkill),
    isError: false,
  });
});

test("a case whose reads show no installed path gets the same neutral one in every arm", () => {
  const bare = structuredClone(f1Case);
  for (const index of bare.skillReads) {
    const part = bare.messages[index]?.content?.[0];
    if (part) part.text = stripSkillHeader(part.text ?? "");
  }
  for (const name of ["old", "new", "none", "minus"] as const) {
    const each = buildArm(bare, name, { old: oldDir, new: newDir }, path.join(scratch, "bare"));
    expect(each.installedDir).toBe("/skills/alpha");
  }
  const old = buildArm(bare, "old", { old: oldDir, new: newDir }, path.join(scratch, "bare"));
  expect(textOf(old.messages[2])).toBe(`[Skill file: /skills/alpha/SKILL.md]\n${f1File(2).trim()}`);
  expect(skillFile(old, "references/x.md").text).toStartWith(
    "[Skill file: /skills/alpha/references/x.md]\n"
  );
});

test("a file the arm lacks gets Oh My Pi's not-found result, in F1's shape", () => {
  const fresh = arm("new");
  const gamma = messages[6];
  expect(textOf(gamma)).toBe("Unknown skill: gamma\nAvailable: alpha, beta");
  const notFound = `File not found: ${path.join(INSTALLED, "references/x.md")}`;
  expect(fresh.messages[4]).toEqual({
    ...gamma,
    toolCallId: messages[4]?.toolCallId,
    timestamp: messages[4]?.timestamp,
    content: [{ type: "text", text: notFound }],
  });
  expect(skillFile(fresh, "references/x.md")).toEqual({ text: notFound, isError: true });
  // An arm without the skill's directory answers as F1's read of the missing skill gamma did.
  const noGamma: Arm = { ...arm("none"), skill: "gamma", profileSkills: f1Case.profile.skills };
  expect(skillFile(noGamma, "SKILL.md")).toEqual({ text: textOf(gamma), isError: true });
  expect(skillFile(arm("none"), "SKILL.md")).toEqual({
    text: "Unknown skill: alpha\nAvailable: beta",
    isError: true,
  });
});

test("none removes each read call of the skill and its result, and leaves the skill out of its profile", () => {
  const none = arm("none");
  expect(none.skillDir).toBeUndefined();
  expect(none.profileSkills.map((skill) => skill.name)).toEqual(["beta"]);
  const parts = none.messages.flatMap((message) => message.content ?? []);
  expect(parts.filter((part) => readIds.includes(part.id))).toEqual([]);
  expect(none.messages.filter((message) => readIds.includes(message.toolCallId))).toEqual([]);
  // The assistant's own words stay; the gamma read, which is no read of alpha, stays whole.
  expect(none.messages).toEqual([
    messages[0],
    { ...messages[1], content: [{ type: "text", text: "Step 1." }] },
    { ...messages[3], content: [{ type: "text", text: "Step 2." }] },
    messages[5],
    messages[6],
  ]);
});

/** acme/tools' alpha, a copy of another source that shares the case skill's name, as the cache
 *  holds the copy a capture installed. */
const TOOLS_ALPHA_MD =
  "---\nname: alpha\ndescription: Use for the tools' own alpha list.\n---\n\n# Alpha of tools\n\nOne line.\n";

/** acme/tools' alpha as a profile lists it, a copy of another source that shares the case skill's
 *  name, under the namespace Oh My Pi gives it (`tools/alpha`); and bare, as the arm of ours lists
 *  it once their copies are gone. */
const TOOLS_ALPHA = {
  repo: "acme/tools",
  commit: "0123456789abcdef",
  path: "skills/alpha",
  name: "alpha",
  namespace: "tools",
};
const TOOLS_ALPHA_BARE = {
  repo: TOOLS_ALPHA.repo,
  commit: TOOLS_ALPHA.commit,
  path: TOOLS_ALPHA.path,
  name: "alpha",
};

// Catches: none listing another source's copy of the name as `<namespace>/<name>`, which Oh My Pi
// lists bare once the tested copy is gone, so a capture refused the body; and a `skill://` read of
// the name in none answered as a skill Oh My Pi does not have, though it resolves to that copy.
test("none leaves out every copy of the skill's source, a namespaced one too, and lists a copy of another source that shares the name under the bare name, which answers a skill:// read of it; each other arm keeps them all as the profile has them", () => {
  const twice = structuredClone(f1Case);
  twice.profile.skills.push(
    // acme/skills' alpha again at another commit, which Oh My Pi lists as pack/alpha.
    {
      repo: "acme/skills",
      commit: "fedcba9876543210",
      path: "skills/alpha",
      name: "alpha",
      namespace: "pack",
    },
    // Another repository's alpha, listed as tools/alpha beside the tested copy.
    {
      repo: "acme/tools",
      commit: "0123456789abcdef",
      path: "skills/alpha",
      name: "alpha",
      namespace: "tools",
    }
  );
  const dirs = { old: oldDir, new: newDir };
  const none = buildArm(twice, "none", dirs, path.join(scratch, "twice"));
  expect(
    none.profileSkills.map((skill) => `${skill.repo} ${skill.namespace ?? ""}/${skill.name}`)
  ).toEqual(["acme/skills /beta", "acme/tools /alpha"]);
  expect(none.relisted).toEqual(["tools/alpha"]);
  // A skill://alpha read is answered from acme/tools' installed copy, at the directory beside
  // the case's other skills, as a read of another listed skill is.
  const copies = [
    { name: "alpha", dir: skillDir("cache-tools-alpha", { "SKILL.md": TOOLS_ALPHA_MD }) },
  ];
  const shown = path.join(path.dirname(INSTALLED), "alpha");
  expect(otherSkillRead(none, copies, "skill://alpha")).toEqual({
    skill: "alpha",
    file: "SKILL.md",
    by: "skill",
    from: "copy",
    text: `[Skill file: ${shown}/SKILL.md]\n${TOOLS_ALPHA_MD.replace(/\n$/, "")}`,
    isError: false,
  });
  expect(otherSkillRead(none, copies, "skill://alpha/references/x.md")).toMatchObject({
    from: "copy",
    text: `File not found: ${shown}/references/x.md`,
    isError: true,
  });
  // Its unknown-skill answer lists the names Oh My Pi lists.
  expect(otherSkillRead(none, copies, "skill://gamma")?.text).toBe(
    "Unknown skill: gamma\nAvailable: beta, alpha"
  );
  for (const name of ["old", "new", "minus", "twin"] as const) {
    const each = buildArm(twice, name, dirs, path.join(scratch, "twice"));
    expect(each.profileSkills).toEqual(twice.profile.skills);
    expect(each.relisted).toBeUndefined();
    // A read of the case's skill is the arm's own, which no other skill answers.
    expect(otherSkillRead(each, copies, "skill://alpha")).toBeUndefined();
  }
});

// Catches: none listing bare the first copy of the name in profile order, where Oh My Pi lists
// bare the copy in the namespace directory it reads first, so the listing names another copy.
test("of two copies of other sources under two namespaces, none lists bare the one whose namespace's directory Oh My Pi reads first, the order of each namespace's first copy, and the other keeps its namespace", () => {
  const kit = { repo: "acme/kit", commit: "0123456789abcdef" };
  const c = structuredClone(f1Case);
  c.profile.skills.push(
    // kit's first copy, so the capture gives Oh My Pi kit's directory before tools'.
    { ...kit, path: "skills/beta", name: "beta", namespace: "kit" },
    // First in profile order of the two alpha copies, in the directory Oh My Pi reads second.
    {
      repo: "acme/tools",
      commit: "0123456789abcdef",
      path: "skills/alpha",
      name: "alpha",
      namespace: "tools",
    },
    { ...kit, path: "skills/alpha", name: "alpha", namespace: "kit" }
  );
  const none = buildArm(c, "none", { old: oldDir, new: newDir }, path.join(scratch, "two-ns"));
  expect(
    none.profileSkills.map((skill) => `${skill.repo} ${skill.namespace ?? ""}/${skill.name}`)
  ).toEqual([
    "acme/skills /beta",
    "acme/kit kit/beta",
    "acme/tools tools/alpha",
    "acme/kit /alpha",
  ]);
  expect(none.relisted).toEqual(["kit/alpha"]);
  // The case's profile is as it was.
  expect(c.profile.skills.at(-1)?.namespace).toBe("kit");
});

test("minus removes the case's line, or the planted line when one is given", () => {
  const minus = arm("minus");
  expect(minus.skillDir).toBe(path.join(scratch, "scratch-minus-", "minus"));
  const less = f1File(2).replace(`${LINE}\n`, "");
  expect(readFileSync(path.join(minus.skillDir ?? "", "SKILL.md"), "utf8")).toBe(less);
  expect(textOf(minus.messages[2])).toBe(readResult("SKILL.md", less));
  expect(textOf(minus.messages[4])).toBe(readResult("references/x.md", f1File(4)));
  // The old directory keeps its text.
  expect(readFileSync(path.join(oldDir, "SKILL.md"), "utf8")).toBe(f1File(2));

  const plantedOld = skillDir("old-planted", {
    "SKILL.md": f1File(2),
    "references/x.md": `${f1File(4)}${PLANTED}\n`,
  });
  const planted = buildArm(
    f1Case,
    "minus",
    { old: plantedOld, new: newDir, plantedLine: PLANTED },
    path.join(scratch, "scratch-planted")
  );
  const dir = planted.skillDir ?? "";
  expect(readFileSync(path.join(dir, "SKILL.md"), "utf8")).toBe(f1File(2));
  expect(readFileSync(path.join(dir, "references/x.md"), "utf8")).toBe(f1File(4));
  expect(() => arm("minus", "A line no file holds.")).toThrow(
    `case c1: the minus arm would equal old: the planted line is in no file of ${oldDir}`
  );
});

test("minus copies each link of the old directory as the file it names, so its edit never writes through a link, and leaves out every copy of the line", () => {
  // The old SKILL.md is a link to a file outside the directory, its reference a relative link.
  const shared = skillDir("linked-shared", { "alpha.md": f1File(2), "x.md": f1File(4) });
  const linked = path.join(scratch, "linked-old");
  mkdirSync(path.join(linked, "references"), { recursive: true });
  symlinkSync(path.join(shared, "alpha.md"), path.join(linked, "SKILL.md"));
  symlinkSync("../../linked-shared/x.md", path.join(linked, "references", "x.md"));
  const dirs = { old: linked, new: newDir };
  const minus = buildArm(f1Case, "minus", dirs, path.join(scratch, "scratch-linked"));
  const dir = minus.skillDir ?? "";
  expect(lstatSync(path.join(dir, "SKILL.md")).isSymbolicLink()).toBe(false);
  expect(readFileSync(path.join(dir, "SKILL.md"), "utf8")).toBe(f1File(2).replace(`${LINE}\n`, ""));
  expect(readFileSync(path.join(dir, "references/x.md"), "utf8")).toBe(f1File(4));
  // The files the links name keep their text.
  expect(readFileSync(path.join(shared, "alpha.md"), "utf8")).toBe(f1File(2));
  const planted = buildArm(
    f1Case,
    "minus",
    { ...dirs, plantedLine: f1File(4).trimEnd().split("\n").at(-1) ?? "" },
    path.join(scratch, "scratch-linked-planted")
  );
  expect(readFileSync(path.join(planted.skillDir ?? "", "references/x.md"), "utf8")).not.toBe(
    f1File(4)
  );
  expect(readFileSync(path.join(shared, "x.md"), "utf8")).toBe(f1File(4));

  // A rule written twice is still the rule: minus leaves out each line that is the case's line.
  const twice = skillDir("twice-old", {
    "SKILL.md": f1File(2).replace(`${LINE}\n`, `${LINE}\n${LINE}\n`),
    "references/x.md": f1File(4),
  });
  const less = buildArm(f1Case, "minus", { old: twice, new: newDir }, path.join(scratch, "twice"));
  expect(readFileSync(path.join(less.skillDir ?? "", "SKILL.md"), "utf8")).toBe(
    f1File(2).replace(`${LINE}\n`, "")
  );
});

// Catches: a link of the old directory that names nothing failing the minus arm with a raw error,
// where old answers a read of it as not found.
test("minus answers a read of a link of the old directory that names nothing as not found, as old does", () => {
  const dangling = skillDir("dangling-old", {
    "SKILL.md": f1File(2),
    "references/x.md": f1File(4),
  });
  symlinkSync("nowhere.md", path.join(dangling, "references", "gone.md"));
  const dirs = { old: dangling, new: newDir };
  const old = buildArm(f1Case, "old", dirs, path.join(scratch, "scratch-dangling"));
  const minus = buildArm(f1Case, "minus", dirs, path.join(scratch, "scratch-dangling"));
  expect(skillFile(minus, "references/gone.md")).toEqual(skillFile(old, "references/gone.md"));
  expect(skillFile(minus, "references/gone.md").isError).toBe(true);
  expect(readFileSync(path.join(minus.skillDir ?? "", "SKILL.md"), "utf8")).toBe(
    f1File(2).replace(`${LINE}\n`, "")
  );
});

// Catches: a link to a directory that holds the old directory copied into itself without end.
test("minus refuses an old directory with a link to a directory that holds it, naming the link", () => {
  const looped = skillDir("looped-old", { "SKILL.md": f1File(2) });
  symlinkSync("..", path.join(looped, "up"));
  expect(() =>
    buildArm(f1Case, "minus", { old: looped, new: newDir }, path.join(scratch, "scratch-looped"))
  ).toThrow(`${path.join(looped, "up")} names a directory that holds ${looped}`);
});

/** An arm's messages as JSON with the text of the skill's files taken out: each read result of
 * them keeps its header line, and the text after it and the tool's record of that text go. */
function outsideFileTexts(each: Arm): string {
  return JSON.stringify(
    each.messages.map((message) => {
      if (!readIds.includes(message.toolCallId)) return message;
      const header = textOf(message).split("\n")[0];
      const details = { ...detailsOf(message), totalLines: 0, displayContent: {} };
      return { ...message, content: [{ type: "text", text: header }], details };
    })
  );
}

test("only the skill's directory differs between arms", () => {
  const dirs = { old: oldDir, new: wholeNewDir };
  const build = (name: ArmName) => buildArm(f1Case, name, dirs, path.join(scratch, "whole"));
  const [old, fresh, minus, none] = [build("old"), build("new"), build("minus"), build("none")];
  // Outside the skill's file texts, headers included, the arms with the skill are byte-identical.
  expect(outsideFileTexts(fresh)).toBe(outsideFileTexts(old));
  expect(outsideFileTexts(minus)).toBe(outsideFileTexts(old));
  for (const index of f1Case.skillReads) {
    for (const each of [old, fresh, minus]) {
      expect(skillHeaderPath(textOf(each.messages[index]))).toBe(
        skillHeaderPath(textOf(messages[index]))
      );
    }
  }
  expect(textOf(fresh.messages[2])).not.toBe(textOf(old.messages[2]));
  expect(textOf(fresh.messages[4])).toBe(readResult("references/x.md", newX));
  // None is the others less the skill's read calls and their results.
  const withoutReads = old.messages.flatMap((message) => {
    if (readIds.includes(message.toolCallId)) return [];
    const kept = (message.content ?? []).filter((part) => !readIds.includes(part.id));
    return [{ ...message, content: kept }];
  });
  expect(JSON.stringify(none.messages)).toBe(JSON.stringify(withoutReads));
  for (const each of [old, fresh, minus, none]) {
    expect(each.installedDir).toBe(INSTALLED);
    expect(each.skill).toBe("alpha");
    // No arm's directory, nor the scratch directory they are under, reaches its messages.
    expect(JSON.stringify(each.messages)).not.toContain(scratch);
  }
  for (const each of [old, fresh, minus]) expect(each.profileSkills).toEqual(f1Case.profile.skills);
  // No arm changes the case it was built from.
  expect(f1Case).toEqual(original);
});

// Catches: a false-loss control whose second arm is not the old text, or is a copy the model can
// tell from it.
test("twin is the old arm again: the old directory, the same messages and profile, its own name", () => {
  const old = arm("old");
  const twin = arm("twin");
  expect(twin.name).toBe("twin");
  expect(twin.skillDir).toBe(old.skillDir ?? "");
  expect(twin.skillDir).toBe(path.resolve(oldDir));
  expect(JSON.stringify(twin.messages)).toBe(JSON.stringify(old.messages));
  expect(twin.profileSkills).toEqual(old.profileSkills);
  expect(twin.installedDir).toBe(old.installedDir);
  expect(f1Case).toEqual(original);
});

// The arm of ours (`--ours`): a skill whose name may differ from the case's skill. `alpha-ours`
// is alpha's directory renamed (its frontmatter name and nothing else changed); `alpha-short`
// holds a SKILL.md of its own and no references/x.md.
const OURS_INSTALLED = path.join(path.dirname(INSTALLED), "alpha-ours");
const SHORT_SKILL =
  "---\nname: alpha-short\ndescription: Use for the alpha checklist, in one line.\n---\n\n# Alpha, short\n\n2. Write each step's result on one line.\n";
const renamedDir = path.join(scratch, "alpha-ours");
const shortDir = path.join(scratch, "alpha-short");
beforeAll(() => {
  skillDir("alpha-ours", {
    "SKILL.md": f1File(2).replace("name: alpha\n", "name: alpha-ours\n"),
    "references/x.md": f1File(4),
  });
  skillDir("alpha-short", { "SKILL.md": SHORT_SKILL });
});

/** The arm `name` of `c` with `ours` (its directory and its name) as the arm of ours. */
function withOurs(c: Case, name: ArmName, ours: { dir: string; name: string }): Arm {
  const dirs = { old: oldDir, new: newDir, ours };
  return buildArm(c, name, dirs, path.join(scratch, `scratch-ours-${ours.name}`));
}
const RENAMED = { dir: renamedDir, name: "alpha-ours" };
const SHORT = { dir: shortDir, name: "alpha-short" };
/** A skill of another source that a profile lists under our name. */
const INCUMBENT = {
  repo: "acme/dotfiles",
  commit: "fedcba9876543210",
  path: "skills/alpha-ours",
  name: "alpha-ours",
};

test("old holds the profile's skills; ours holds them with ours in place of theirs, a skill of our name replaced, not doubled; none holds them less theirs", () => {
  const [alpha, beta] = f1Case.profile.skills;
  expect(withOurs(f1Case, "old", RENAMED).profileSkills).toEqual(f1Case.profile.skills);
  const ours = withOurs(f1Case, "ours", RENAMED);
  expect(ours.name).toBe("ours");
  expect(ours.skill).toBe("alpha-ours");
  expect(ours.skillDir).toBe(path.resolve(renamedDir));
  expect(ours.installedDir).toBe(OURS_INSTALLED);
  expect(ours.profileSkills).toEqual([{ ...alpha, name: "alpha-ours" }, beta]);
  expect(ours.ours).toEqual({ theirs: "alpha", replacedOwn: false, removedReads: [] });
  expect(withOurs(f1Case, "none", RENAMED).profileSkills).toEqual([beta]);

  // A profile that holds a skill of our name already: the --ours text replaces that entry, so
  // the name is installed once, and the arm records that it replaced it.
  const holding: Case = {
    ...f1Case,
    profile: { ...f1Case.profile, skills: [...f1Case.profile.skills, INCUMBENT] },
  };
  const bundled = withOurs(holding, "ours", RENAMED);
  expect(bundled.profileSkills).toEqual([beta, INCUMBENT]);
  expect(bundled.ours?.replacedOwn).toBe(true);
  expect(withOurs(holding, "old", RENAMED).profileSkills).toEqual(holding.profile.skills);
  expect(withOurs(holding, "none", RENAMED).profileSkills).toEqual([beta, INCUMBENT]);

  // Ours under their skill's own name stands in their skill's place, as the cut's text does.
  const same = withOurs(f1Case, "ours", { dir: renamedDir, name: "alpha" });
  expect(same.profileSkills).toEqual(f1Case.profile.skills);
  expect(same.ours?.replacedOwn).toBe(false);
  expect(f1Case).toEqual(original);
});

// Catches: a namespaced copy of their skill's source kept in the arm of ours, where its old text
// stays readable at another path, or ours listed twice.
test("the ours arm leaves out every copy of their skill's source, a namespaced one too, and lists ours once under its own name", () => {
  const [alpha, beta] = f1Case.profile.skills;
  const copies = structuredClone(f1Case);
  copies.profile.skills.push(
    // acme/skills' alpha again at another commit, which Oh My Pi lists as pack/alpha.
    {
      repo: "acme/skills",
      commit: "fedcba9876543210",
      path: "skills/alpha",
      name: "alpha",
      namespace: "pack",
    },
    // Another repository's alpha, listed as tools/alpha: once their copies are gone, the arm
    // lists it under the bare name, as none does.
    TOOLS_ALPHA
  );
  const ours = withOurs(copies, "ours", RENAMED);
  expect(ours.profileSkills).toEqual([{ ...alpha, name: "alpha-ours" }, beta, TOOLS_ALPHA_BARE]);
  expect(ours.ours?.replacedOwn).toBe(false);
  // A read of a skill the arm lacks is answered with the names Oh My Pi lists, ours once under
  // its own name.
  expect(unknownSkill(ours, "gamma").text).toBe(
    "Unknown skill: gamma\nAvailable: alpha-ours, beta, alpha"
  );

  // A profile that lists a skill of another source under our name keeps it, the --ours text in
  // its place, and no copy of theirs.
  copies.profile.skills.push(INCUMBENT);
  const bundled = withOurs(copies, "ours", RENAMED);
  expect(bundled.profileSkills).toEqual([beta, TOOLS_ALPHA_BARE, INCUMBENT]);
  expect(bundled.ours?.replacedOwn).toBe(true);
  expect(f1Case).toEqual(original);
});

// Catches: the arm of ours keeping another source's copy of their skill's name as
// `<namespace>/<name>`, which Oh My Pi lists bare once their copies are gone, so a capture refused
// the body; a `skill://<theirs>` read in ours answered as a skill Oh My Pi does not have, though
// it resolves to that copy; or a namespaced copy of our name listed bare beside ours.
test("the ours arm lists a copy of another source that shares their skill's name under the bare name, as none does, records it, and answers a skill:// read of their name from it; a namespaced copy of our name keeps its namespace; a case with no such copy records none and lists ours and the profile's other skills byte for byte", () => {
  const [alpha, beta] = f1Case.profile.skills;
  const pack = {
    repo: "acme/skills",
    commit: "fedcba9876543210",
    path: "skills/alpha",
    name: "alpha",
    namespace: "pack",
  };
  const relisting = structuredClone(f1Case);
  relisting.profile.skills.push(pack, TOOLS_ALPHA);
  const ours = withOurs(relisting, "ours", RENAMED);
  expect(ours.profileSkills).toEqual([{ ...alpha, name: "alpha-ours" }, beta, TOOLS_ALPHA_BARE]);
  expect(ours.relisted).toEqual(["tools/alpha"]);
  expect(caseNameCopy(ours)).toEqual(TOOLS_ALPHA_BARE);
  // A skill://alpha read is answered from acme/tools' installed copy, at the directory beside
  // ours, as none answers it.
  const copies = [
    { name: "alpha", dir: skillDir("cache-tools-alpha-ours", { "SKILL.md": TOOLS_ALPHA_MD }) },
  ];
  const shown = path.join(path.dirname(INSTALLED), "alpha");
  const fromTools = {
    skill: "alpha",
    file: "SKILL.md",
    by: "skill" as const,
    from: "copy" as const,
    text: `[Skill file: ${shown}/SKILL.md]\n${TOOLS_ALPHA_MD.replace(/\n$/, "")}`,
    isError: false,
  };
  expect(otherSkillRead(ours, copies, "skill://alpha")).toEqual(fromTools);
  expect(otherSkillRead(withOurs(relisting, "none", RENAMED), copies, "skill://alpha")).toEqual(
    fromTools
  );
  expect(otherSkillRead(ours, copies, "skill://alpha/references/x.md")).toMatchObject({
    from: "copy",
    text: `File not found: ${shown}/references/x.md`,
    isError: true,
  });
  // A read of ours is still the arm's own, from our directory.
  expect(otherSkillRead(ours, copies, "skill://alpha-ours")).toBeUndefined();
  expect(skillFile(ours, "SKILL.md").text).toStartWith(
    `[Skill file: ${OURS_INSTALLED}/SKILL.md]\n`
  );
  // The history is as it is without that copy: each read of theirs became a read of ours.
  const withoutTools = structuredClone(f1Case);
  withoutTools.profile.skills.push(pack);
  expect(ours.messages).toEqual(withOurs(withoutTools, "ours", RENAMED).messages);

  // A profile that lists a skill of another source under our name: the same, beside it.
  const holding = structuredClone(relisting);
  holding.profile.skills.push(INCUMBENT);
  const bundled = withOurs(holding, "ours", RENAMED);
  expect(bundled.profileSkills).toEqual([beta, TOOLS_ALPHA_BARE, INCUMBENT]);
  expect(bundled.relisted).toEqual(["tools/alpha"]);
  expect(otherSkillRead(bundled, copies, "skill://alpha")).toEqual(fromTools);

  // Ours under their skill's own name holds that name bare in skills/, which Oh My Pi reads
  // first, so tools/alpha keeps its namespace and a read of alpha is the arm's own.
  const same = withOurs(relisting, "ours", { dir: renamedDir, name: "alpha" });
  expect(same.profileSkills).toEqual([alpha, beta, TOOLS_ALPHA]);
  expect(same.relisted).toBeUndefined();
  expect(caseNameCopy(same)).toBeUndefined();
  expect(otherSkillRead(same, copies, "skill://alpha")).toBeUndefined();
  // So does ours under a name a copy of another source holds under a namespace.
  const kit = {
    repo: "acme/kit",
    commit: "0123456789abcdef",
    path: "skills/alpha-ours",
    name: "alpha-ours",
    namespace: "kit",
  };
  const sharing = structuredClone(f1Case);
  sharing.profile.skills.push(kit);
  const shared = withOurs(sharing, "ours", RENAMED);
  expect(shared.profileSkills).toEqual([{ ...alpha, name: "alpha-ours" }, beta, kit]);
  expect(shared.relisted).toBeUndefined();
  expect(shared.ours?.replacedOwn).toBe(false);

  // With no copy of another source under their name, the arm records none, lists ours and the
  // profile's other skills byte for byte, and a read of theirs is a skill the arm does not install.
  for (const c of [f1Case, withoutTools]) {
    const plain = withOurs(c, "ours", RENAMED);
    expect(Object.hasOwn(plain, "relisted")).toBe(false);
    expect(JSON.stringify(plain.profileSkills)).toBe(
      JSON.stringify([{ ...alpha, name: "alpha-ours" }, beta])
    );
    expect(caseNameCopy(plain)).toBeUndefined();
    expect(otherSkillRead(plain, copies, "skill://alpha")).toMatchObject({
      from: "absent",
      text: "Unknown skill: alpha\nAvailable: alpha-ours, beta",
      isError: true,
    });
  }
  expect(f1Case).toEqual(original);
});

test("each history read mapped by its path inside the skill, or removed when ours lacks the file", () => {
  const short = withOurs(f1Case, "ours", SHORT);
  const shortInstalled = path.join(path.dirname(INSTALLED), "alpha-short");
  const [call] = (messages[1]?.content ?? []).filter((part) => part.type === "toolCall");
  const read = messages[2];
  const shown = path.join(shortInstalled, "SKILL.md");
  const body = SHORT_SKILL.replace(/\n$/, "");
  expect(short.messages).toEqual([
    messages[0],
    {
      ...messages[1],
      content: [
        { type: "text", text: "Step 1." },
        { ...call, arguments: { ...Object(call?.arguments), path: "skill://alpha-short" } },
      ],
    },
    {
      ...read,
      content: [{ type: "text", text: `[Skill file: ${shown}]\n${body}` }],
      details: {
        ...detailsOf(read),
        resolvedPath: shown,
        totalLines: 8,
        displayContent: { text: body, startLine: 1, lineNumbers: [1, 2, 3, 4, 5, 6, 7, 8] },
        meta: { source: { type: "internal", value: "skill://alpha-short" } },
      },
    },
    // references/x.md: ours has no such file, so its read and result go, as none removes them.
    { ...messages[3], content: [{ type: "text", text: "Step 2." }] },
    messages[5],
    messages[6],
  ]);
  // A new read, as replay asks one, is answered from our directory at our installed path.
  expect(skillFile(short, "SKILL.md")).toEqual({
    text: `[Skill file: ${shown}]\n${body}`,
    isError: false,
  });
  expect(JSON.stringify(short.messages)).not.toContain(scratch);
});

test("a renamed unchanged copy gives old's texts", () => {
  const old = withOurs(f1Case, "old", RENAMED);
  const ours = withOurs(f1Case, "ours", RENAMED);
  // But for its name (in the read's URL, the installed path and the frontmatter), every text and
  // record of the history is old's, byte for byte, and so is the profile.
  expect(JSON.stringify(ours.messages)).not.toBe(JSON.stringify(old.messages));
  expect(JSON.stringify(ours.messages).replaceAll("alpha-ours", "alpha")).toBe(
    JSON.stringify(old.messages)
  );
  expect(JSON.stringify(ours.profileSkills).replaceAll("alpha-ours", "alpha")).toBe(
    JSON.stringify(old.profileSkills)
  );
  expect(skillFile(ours, "references/x.md").text.replaceAll("alpha-ours", "alpha")).toBe(
    skillFile(old, "references/x.md").text
  );
});

test("the ours arm records each history read it removed for want of our file at that path", () => {
  expect(withOurs(f1Case, "ours", SHORT).ours?.removedReads).toEqual(["references/x.md"]);
  expect(withOurs(f1Case, "ours", RENAMED).ours?.removedReads).toEqual([]);
  // No other arm carries the record.
  for (const name of ["old", "new", "none", "minus", "twin"] as const) {
    expect(withOurs(f1Case, name, SHORT).ours).toBeUndefined();
  }
  expect(() => buildArm(f1Case, "ours", { old: oldDir }, path.join(scratch, "no-ours"))).toThrow(
    "case c1: the ours arm needs --ours"
  );
});

// Catches: an arm of a written case that changes its task or the read's call, or a listing that
// differs between arms in more than the skill.
test("a written case's arms differ only in the skill's read and listing", () => {
  const task: SessionMessage = {
    role: "user",
    content: [{ type: "text", text: "Write the alpha checklist of the fixture project." }],
  };
  const call: SessionMessage = {
    role: "assistant",
    content: [
      { type: "toolCall", id: "written_read", name: "read", arguments: { path: "skill://alpha" } },
    ],
  };
  const read: SessionMessage = {
    role: "toolResult",
    toolCallId: "written_read",
    toolName: "read",
    isError: false,
    content: [{ type: "text", text: readResult("SKILL.md", f1File(2)) }],
  };
  const written: Case = {
    ...f1Case,
    id: "c-written",
    messages: [task, call, read],
    skillReads: [2],
    source: { ...f1Case.source, kind: "written" },
  };
  const [old, fresh, minus, twin, none, ours] = (
    ["old", "new", "minus", "twin", "none", "ours"] as const
  ).map((name) => withOurs(written, name, RENAMED));
  // The task is the same message in every arm.
  for (const each of [old, fresh, minus, twin, none, ours]) expect(each?.messages[0]).toEqual(task);
  // Each arm with their skill keeps the call, and answers the read from its own text at the
  // installed path.
  for (const each of [old, fresh, minus, twin]) {
    expect(each?.messages).toHaveLength(3);
    expect(each?.messages[1]).toEqual(call);
    expect(skillHeaderPath(textOf(each?.messages[2]))).toBe(path.join(INSTALLED, "SKILL.md"));
    expect(each?.profileSkills).toEqual(written.profile.skills);
  }
  expect(textOf(old?.messages[2])).toBe(readResult("SKILL.md", f1File(2)));
  expect(textOf(fresh?.messages[2])).toBe(readResult("SKILL.md", newSkill));
  expect(textOf(minus?.messages[2])).toBe(
    readResult("SKILL.md", f1File(2).replace(`${LINE}\n`, ""))
  );
  expect(JSON.stringify(twin?.messages)).toBe(JSON.stringify(old?.messages));
  // No skill: the task alone, and the profile less the skill.
  const [alpha, beta] = written.profile.skills;
  expect(none?.messages).toEqual([task]);
  expect(none?.profileSkills).toEqual([beta]);
  // Ours: the read of our skill at the same path inside it, and ours listed in their place.
  expect(ours?.messages[1]).toEqual({
    ...call,
    content: [
      {
        type: "toolCall",
        id: "written_read",
        name: "read",
        arguments: { path: "skill://alpha-ours" },
      },
    ],
  });
  expect(skillHeaderPath(textOf(ours?.messages[2]))).toBe(path.join(OURS_INSTALLED, "SKILL.md"));
  expect(ours?.profileSkills).toEqual([{ ...alpha, name: "alpha-ours" }, beta]);
});

// A skill that reaches a session other than by its listing line and its reads (arms.ts's header).

/** Where the made-up subagent's Oh My Pi autoloaded alpha from. */
const AUTOLOADED_PATH = `${INSTALLED}/SKILL.md`;

/** The text of Oh My Pi's autoloaded block of the SKILL.md text `skill`, for a text its prompt
 *  format leaves as it is (F1's alpha): the text less its frontmatter, trimmed, a blank line,
 *  `---`, a blank line and `Skill: <path>` (prompts/skills/autoload.md at the pin). */
function autoloadedOf(skill: string): string {
  return `${skill.slice(skill.indexOf("\n---", 3) + 4).trim()}\n\n---\n\nSkill: ${AUTOLOADED_PATH}`;
}

/** Alpha autoloaded into a made-up subagent before its task, as Oh My Pi's task executor sends
 *  it (a `skill-prompt` custom message the user did not invoke), from the text F1 read. */
const autoloaded: SessionMessage = {
  role: "custom",
  customType: "skill-prompt",
  content: [{ type: "text", text: autoloadedOf(f1File(2)) }],
  display: false,
  details: { name: "alpha", path: AUTOLOADED_PATH },
  attribution: "agent",
  timestamp: 1795139983600,
};

/** F1's case with that block before its messages, in a subagent's context. */
function autoloadCase(id: string, block: SessionMessage = autoloaded): Case {
  return {
    ...structuredClone(f1Case),
    id,
    context: "subagent:alpha-runner",
    messages: [block, ...structuredClone(f1Case.messages)],
    skillReads: f1Case.skillReads.map((index) => index + 1),
  };
}

// Catches: an arm that changes only the listing line and the read results of a skill Oh My Pi
// autoloads into a subagent, so every arm sends the recorded text of it.
test("an autoloaded block of the skill carries each arm's own text under its recorded path, and none leaves it out", () => {
  const dirs = { old: oldDir, new: newDir };
  const c = autoloadCase("c-autoload");
  // new and minus first: the old arm's text is the recorded one.
  for (const name of ["new", "minus", "old", "twin"] as const) {
    const built = buildArm(c, name, dirs, path.join(scratch, `autoload-${name}`));
    const text = readFileSync(path.join(built.skillDir ?? "", "SKILL.md"), "utf8");
    expect(built.messages[0]).toEqual({
      ...autoloaded,
      content: [{ type: "text", text: autoloadedOf(text) }],
    });
    // Every other message is the arm's of the case without the block.
    const without = buildArm(f1Case, name, dirs, path.join(scratch, `autoload-${name}-without`));
    expect(built.messages.slice(1)).toEqual(without.messages);
    expect(built.injections).toEqual([{ by: "autoload", path: AUTOLOADED_PATH, at: 0 }]);
  }
  const fresh = buildArm(c, "new", dirs, path.join(scratch, "autoload-new"));
  expect(textOf(fresh.messages[0])).toContain("2. Put each step's result on a line of its own.");
  expect(textOf(fresh.messages[0])).not.toContain(LINE);
  const minus = buildArm(c, "minus", dirs, path.join(scratch, "autoload-minus"));
  expect(textOf(minus.messages[0])).not.toContain(LINE);
  const none = buildArm(c, "none", dirs, path.join(scratch, "autoload-none"));
  expect(none.messages).toEqual(arm("none").messages);
  expect(JSON.stringify(none.messages)).not.toContain(`Skill: ${AUTOLOADED_PATH}`);
  expect(none.injections).toEqual([{ by: "autoload", path: AUTOLOADED_PATH, at: null }]);
  expect(c.messages[0]).toEqual(autoloaded);
});

// Catches: an autoloaded block rebuilt from the file's text as it is, where Oh My Pi renders it
// through its prompt format (a table of thermonuclear-deep-review's SKILL.md, 2026-11-22).
test("an autoloaded block's text is the SKILL.md as Oh My Pi's prompt format leaves it: a table's cells unpadded, a run of blank lines dropped, no blank line before a closing tag, a fence's lines kept", () => {
  const formatted = skillDir("formatted", {
    "SKILL.md": [
      "---",
      "name: alpha",
      "description: Use for the made-up alpha table.",
      "---",
      "",
      "# Alpha   ",
      "",
      "",
      "",
      "Rules:",
      "",
      "| Step | What |",
      "| :--- | ---: |",
      "|  1  |  Read  |",
      "",
      "<rules>",
      "Keep it.",
      "",
      "</rules>",
      "",
      "```",
      "kept   ",
      "",
      "",
      "",
      "as is",
      "```",
      "",
    ].join("\n"),
  });
  const built = buildArm(
    autoloadCase("c-format"),
    "new",
    { old: oldDir, new: formatted },
    path.join(scratch, "format")
  );
  expect(textOf(built.messages[0])).toBe(
    [
      "# Alpha",
      "Rules:",
      "",
      "|Step|What|",
      "|:---|---:|",
      "|1|Read|",
      "",
      "<rules>",
      "Keep it.",
      "</rules>",
      "",
      "```",
      "kept",
      "",
      "",
      "",
      "as is",
      "```",
      "",
      "---",
      "",
      `Skill: ${AUTOLOADED_PATH}`,
    ].join("\n")
  );
});

/** A made-up dispatch-first, as the Envoy plugin reads it, and the cut of it. */
const DISPATCH_FIRST =
  "---\nname: dispatch-first\ndescription: Use when the made-up session has Dispatch.\n---\n\n# Dispatch first\n\nSearch before you act.\nAsk only what no search answers.\n";
const DISPATCH_FIRST_CUT = DISPATCH_FIRST.replace(
  "Search before you act.",
  "Search the made-up tracker before you act."
);
const dispatchScratch = temporaryDirectory("skill-cuts-arms-dispatch", { real: true });

/** The old and new directories of dispatch-first under dispatchScratch: DISPATCH_FIRST, and its
 *  cut. */
function dispatchDirs(): { old: string; new: string } {
  const dirs = { old: path.join(dispatchScratch, "old"), new: path.join(dispatchScratch, "new") };
  mkdirSync(dirs.old, { recursive: true });
  mkdirSync(dirs.new, { recursive: true });
  writeFileSync(path.join(dirs.old, "SKILL.md"), DISPATCH_FIRST);
  writeFileSync(path.join(dirs.new, "SKILL.md"), DISPATCH_FIRST_CUT);
  return dirs;
}

/** A case of dispatch-first in a profile that installs the Envoy plugin, whose session read the
 *  skill once; `before` goes before its messages. */
function dispatchCase(id: string, before: SessionMessage[] = [], plugin = "pi-envoy"): Case {
  const messages: SessionMessage[] = [
    ...before,
    {
      role: "user",
      content: [{ type: "text", text: "Plan the made-up change." }],
      attribution: "user",
      timestamp: 1,
    },
    {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "toolu_df",
          name: "read",
          arguments: { path: "skill://dispatch-first" },
        },
      ],
      timestamp: 2,
    },
    {
      role: "toolResult",
      toolCallId: "toolu_df",
      toolName: "read",
      content: [
        {
          type: "text",
          text: `[Skill file: /skills/dispatch-first/SKILL.md]\n${DISPATCH_FIRST.trimEnd()}`,
        },
      ],
      isError: false,
      timestamp: 3,
    },
  ];
  return {
    ...structuredClone(f1Case),
    id,
    skill: "dispatch-first",
    line: { text: "Search before you act.", lineNumber: 8, lineCount: 9 },
    profile: {
      skills: [
        {
          repo: "acme/skills",
          commit: "0123456789abcdef",
          path: "skills/dispatch-first",
          name: "dispatch-first",
        },
        { repo: "acme/skills", commit: "0123456789abcdef", path: "skills/beta", name: "beta" },
      ],
      plugins: [
        {
          repo: "acme/legion",
          commit: "0123456789abcdef",
          path: `packages/${plugin}`,
          name: plugin,
        },
      ],
      contextFiles: [],
    },
    messages,
    skillReads: [before.length + 2],
  };
}

// Catches: a dispatch-first case whose arms differ only in its listing line and read results,
// though the Envoy plugin puts the skill's text into every request of the session, and no
// transcript keeps that block.
test("the Envoy plugin's dispatch-first block, which no transcript holds, carries each arm's own text where the plugin puts it, and none has none", () => {
  const dirs = dispatchDirs();
  const c = dispatchCase("c-dispatch");
  const blocks = new Map<string, string>();
  for (const name of ["old", "new", "minus", "twin"] as const) {
    const built = buildArm(c, name, dirs, path.join(dispatchScratch, name));
    const text = readDispatchFirstContext(path.join(built.skillDir ?? "", "SKILL.md"));
    expect(built.messages[0]).toEqual({
      role: "user",
      content: [{ type: "text", text }],
      timestamp: 0,
    });
    expect(built.messages).toHaveLength(c.messages.length + 1);
    expect(built.messages.slice(1).map((message) => message.role)).toEqual(
      c.messages.map((message) => message.role)
    );
    expect(built.injections).toEqual([
      { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 },
    ]);
    blocks.set(name, text);
  }
  expect(blocks.get("old")).toStartWith("<dispatch-first-skill>\n");
  expect(blocks.get("old")).toContain("Search before you act.");
  expect(blocks.get("new")).toContain("Search the made-up tracker before you act.");
  expect(blocks.get("minus")).not.toContain("Search before you act.");
  expect(blocks.get("twin")).toBe(blocks.get("old") ?? "");
  const none = buildArm(c, "none", dirs, path.join(dispatchScratch, "none"));
  expect(JSON.stringify(none.messages)).not.toContain("<dispatch-first-skill>");
  expect(none.injections).toEqual([
    { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: null },
  ]);
  expect(none.profileSkills.map((skill) => skill.name)).toEqual(["beta"]);
  // After the leading compaction summary, where the plugin puts it.
  const summary: SessionMessage = { role: "compactionSummary", summary: "Earlier work." };
  const compacted = buildArm(
    dispatchCase("c-compacted", [summary]),
    "old",
    dirs,
    path.join(dispatchScratch, "compacted")
  );
  expect(compacted.messages.map((message) => message.role).slice(0, 3)).toEqual([
    "compactionSummary",
    "user",
    "user",
  ]);
  expect(textOf(compacted.messages[1])).toBe(blocks.get("old") ?? "");
  // A profile without the plugin gets no block.
  const without = buildArm(
    dispatchCase("c-no-plugin", [], "pi-other"),
    "old",
    dirs,
    path.join(dispatchScratch, "no-plugin")
  );
  expect(JSON.stringify(without.messages)).not.toContain("<dispatch-first-skill>");
  expect(without.injections).toEqual([]);
});

/** The autoloaded block as a `/skill:` prompt the user invoked. */
const invoked: SessionMessage = { ...autoloaded, attribution: "user", display: true };
/** The autoloaded block with the user's words after it. */
const withArgs: SessionMessage = {
  ...autoloaded,
  content: [{ type: "text", text: `${autoloadedOf(f1File(2))}\nUser: focus on step 2` }],
};
/** A user message that opens with the plugin's marker, quoting its block. */
const quoted: SessionMessage = {
  role: "user",
  content: [{ type: "text", text: "<dispatch-first-skill>\nA quoted block." }],
};

// Catches: a case run with the recorded text in every arm because its block of the skill is in a
// form the arms do not rebuild.
test("a case whose messages hold a block of its skill in a form the arms cannot rebuild is refused, naming the case", () => {
  const dirs = { old: oldDir, new: newDir };
  expect(() =>
    buildArm(autoloadCase("c-invoked", invoked), "old", dirs, path.join(scratch, "invoked"))
  ).toThrow(
    "case c-invoked: its message 0 is a skill-prompt message of alpha, one the user invoked, not in the form of Oh My Pi's autoloaded block"
  );
  expect(() =>
    buildArm(autoloadCase("c-args", withArgs), "new", dirs, path.join(scratch, "args"))
  ).toThrow("case c-args: its message 0 is a skill-prompt message of alpha not in the form");
  const c = dispatchCase("c-quoted");
  expect(() =>
    buildArm(
      { ...c, messages: [...c.messages, quoted] },
      "none",
      dirs,
      path.join(dispatchScratch, "quoted")
    )
  ).toThrow(
    "case c-quoted: its message 3 opens with <dispatch-first-skill> away from where the pi-envoy plugin puts its block (message 0, after the leading compaction summaries)"
  );
});

// Catches: a refused case whose reasons a caller can tell apart only by their wording.
test("each reason a case's block cannot be rebuilt has its kind, injectionFault joins their texts with \"; \", and buildArm's refusal sets no kind", () => {
  const base = autoloadCase("c-kinds", invoked);
  const c: Case = {
    ...base,
    messages: [invoked, withArgs, ...base.messages.slice(1)],
    skillReads: base.skillReads.map((index) => index + 1),
  };
  const form = `not in the form of Oh My Pi's autoloaded block (one text part that ends "Skill: <its details.path>")`;
  expect(injectionFaults(c)).toEqual([
    {
      kind: "user-invoked",
      text: `its message 0 is a skill-prompt message of alpha, one the user invoked, ${form}`,
    },
    { kind: "other-shape", text: `its message 1 is a skill-prompt message of alpha ${form}` },
  ]);
  expect(injectionFault(c)).toBe(
    `its message 0 is a skill-prompt message of alpha, one the user invoked, ${form}; its message 1 is a skill-prompt message of alpha ${form}`
  );
  const misplaced = dispatchCase("c-misplaced");
  expect(injectionFaults({ ...misplaced, messages: [...misplaced.messages, quoted] })).toEqual([
    {
      kind: "plugin-block-misplaced",
      text: "its message 3 opens with <dispatch-first-skill> away from where the pi-envoy plugin puts its block (message 0, after the leading compaction summaries)",
    },
  ]);
  expect(injectionFaults(f1Case)).toEqual([]);
  expect(injectionFault(f1Case)).toBeUndefined();
  // buildArm's refusal carries no kind (injectionFaults gives each); a capture's carries
  // `autoload-no-block` (capture.ts), which cli.test.ts checks end to end (its case c-autoload,
  // dropped as `injection-unrebuildable: autoload-no-block`).
  const refusal = (() => {
    try {
      buildArm(c, "old", { old: oldDir, new: newDir }, path.join(scratch, "kinds"));
    } catch (error) {
      return error;
    }
    return undefined;
  })();
  expect(refusal).toBeInstanceOf(InjectionRefused);
  expect(refusal instanceof InjectionRefused ? refusal.kind : "not thrown").toBeUndefined();
});

/** A tool call of `name` with `args`, alone in an assistant message. */
function toolCall(name: string, args: Record<string, unknown>): SessionMessage {
  return {
    role: "assistant",
    content: [{ type: "toolCall", id: `toolu_${name}`, name, arguments: args }],
  };
}

/** F1's case in `context`, `more` after its seven messages (from message 7), with `evidence`. */
function evidenceCase(
  context: string,
  more: SessionMessage[] = [],
  evidence?: Case["evidence"]
): Case {
  return {
    ...structuredClone(f1Case),
    context,
    messages: [...structuredClone(f1Case.messages), ...more],
    ...(evidence === undefined ? {} : { evidence }),
  };
}

/** The evidence item build.ts writes for a subagent's session, of the store whose context is
 *  `store`. */
function subagentEvidence(store: string): Case["evidence"] {
  return [
    {
      item: "subagent alpha-runner",
      source: `the session's session_init line names the agent alpha-runner; its parent session is another transcript, and the store's context is ${store}`,
    },
  ];
}

// Catches: a Legion role's case taken to have had Dispatch on the assumption a dev box's rests
// on, though its context, or its evidence for a subagent, says it is a Legion role's.
test("a Legion role's session, and a subagent of one by its evidence, had Dispatch from its role", () => {
  expect(dispatchOf(evidenceCase("legion:acme:implementer"))).toEqual({
    from: "legion-role",
    why: "its context legion:acme:implementer is a Legion role's session, and a Legion role works its issue through Dispatch",
  });
  expect(
    dispatchOf(evidenceCase("subagent:alpha-runner", [], subagentEvidence("legion:acme")))
  ).toEqual({
    from: "legion-role",
    why: `it is the session of the subagent alpha-runner of a Legion role's session: its evidence item "subagent alpha-runner" says the store's context is legion:acme`,
  });
  // A Legion role's answer comes first, whatever its messages show.
  const asked = toolCall("bash", { command: "dispatch search --query 'alpha list'" });
  expect(dispatchOf(evidenceCase("legion:acme:tester", [asked])).from).toBe("legion-role");
});

// Catches: a dev box's case that shows its Dispatch in its transcript counted as resting on the
// assumption, and a mention of `dispatch` that runs nothing counted as a call.
test("a session whose messages hold the dispatch-first block or a Dispatch call had Dispatch from its transcript, and a mention that runs nothing is no call", () => {
  const hook: SessionMessage = {
    role: "user",
    content: [
      {
        type: "text",
        text: "<system-reminder>\nSessionStart hook additional context: <dispatch-first-skill>\nThis session has Dispatch, so the dispatch-first skill is loaded for it. Follow it.\n</dispatch-first-skill>\n</system-reminder>",
      },
    ],
  };
  const shows: [SessionMessage, string][] = [
    [
      hook,
      "its message 7 holds the dispatch-first block (<dispatch-first-skill>), which a host puts only into a session with Dispatch",
    ],
    [
      toolCall("dispatch_ask", { issue: "K-1", question: "Ship?" }),
      "its message 7 calls the Dispatch tool dispatch_ask",
    ],
    [
      toolCall("write", { path: "xd://dispatch_comment", content: '{"issue":"K-1","body":"b"}' }),
      "its message 7 writes the Dispatch tool device xd://dispatch_comment",
    ],
    [
      toolCall("bash", { command: "cd /work && dispatch search --query 'alpha list'" }),
      "its message 7 runs `dispatch search` in a bash call",
    ],
    [
      toolCall("bash", { command: "out=$(DISPATCH_HOST=omp dispatch read --issue K-1)" }),
      "its message 7 runs `dispatch read` in a bash call",
    ],
  ];
  for (const [message, why] of shows) {
    expect(dispatchOf(evidenceCase("devbox", [message]))).toEqual({ from: "transcript", why });
  }
  // A subagent of a dev box's session shows it the same way.
  expect(
    dispatchOf(evidenceCase("subagent:alpha-runner", [hook], subagentEvidence("devbox"))).from
  ).toBe("transcript");
  const mentions: SessionMessage[] = [
    // A read of a file that quotes the marker.
    {
      role: "toolResult",
      toolCallId: "toolu_read",
      toolName: "read",
      content: [
        { type: "text", text: 'export const DISPATCH_FIRST_MARKER = "<dispatch-first-skill>";' },
      ],
    },
    toolCall("bash", { command: "kubectl --context dispatch get pods" }),
    toolCall("bash", { command: 'echo "dispatch read --issue K-1"' }),
    toolCall("bash", { command: "cat > notes.md <<'EOF'\ndispatch read --issue K-1\nEOF" }),
    toolCall("bash", { command: "ls /work/dispatch" }),
    toolCall("read", { path: "skill://dispatch-first" }),
  ];
  for (const message of mentions) {
    expect(dispatchOf(evidenceCase("devbox", [message])).from).toBe("assumed");
  }
});

// Catches: a dev box's case that shows no Dispatch given an answer that hides that it rests on
// the runner's assumption.
test("a dev box's session that shows neither is assumed to have had Dispatch, and says so", () => {
  expect(dispatchOf(f1Case)).toEqual({
    from: "assumed",
    why: "its context is devbox, and none of its messages holds the dispatch-first block or a Dispatch call, so the runner takes it that the session had Dispatch",
  });
  const assumed: DispatchAnswer = {
    from: "assumed",
    why: "it is the session of the subagent alpha-runner, whose evidence names no Legion store, and none of its messages holds the dispatch-first block or a Dispatch call, so the runner takes it that the session had Dispatch",
  };
  expect(dispatchOf(evidenceCase("subagent:alpha-runner", [], subagentEvidence("devbox")))).toEqual(
    assumed
  );
  expect(dispatchOf(evidenceCase("subagent:alpha-runner"))).toEqual(assumed);
});

// Catches: an arm that does not carry its case's answer, so a capture cannot record what the
// plugin's block rests on, or a block left out on an answer other than the assumption.
test("each arm carries its case's answer, and the plugin's block goes in on each of the three answers", () => {
  const dirs = dispatchDirs();
  const asked = toolCall("bash", { command: "dispatch search --query 'made-up change'" });
  const cases: [Case, DispatchAnswer["from"]][] = [
    [{ ...dispatchCase("c-legion"), context: "legion:acme:implementer" }, "legion-role"],
    [dispatchCase("c-transcript", [asked]), "transcript"],
    [dispatchCase("c-assumed"), "assumed"],
  ];
  for (const [c, from] of cases) {
    for (const name of ["old", "new", "minus", "twin", "none"] as const) {
      const built = buildArm(c, name, dirs, path.join(dispatchScratch, `answer-${c.id}-${name}`));
      expect({ arm: name, dispatch: built.dispatch }).toEqual({
        arm: name,
        dispatch: dispatchOf(c),
      });
      expect(built.dispatch?.from).toBe(from);
      expect(built.injections).toEqual([
        {
          by: "plugin",
          plugin: "pi-envoy",
          name: "dispatch-first",
          at: name === "none" ? null : 0,
        },
      ]);
    }
  }
  // Every case gets its answer, a skill no plugin puts in included.
  expect(arm("old").dispatch).toEqual(dispatchOf(f1Case));
});

// Catches: the arm of ours sending their skill's text, name or path in a block that reaches the
// session beside its listing line and its reads, where every other arm sends its own text.
test("in the arm of ours, an autoloaded block is ours under our name and installed path, the plugin's block is the plugin's of our text, and the arm carries its case's answer", () => {
  const c = autoloadCase("c-autoload-ours");
  const ours = withOurs(c, "ours", RENAMED);
  const file = path.join(OURS_INSTALLED, "SKILL.md");
  const text = readFileSync(path.join(renamedDir, "SKILL.md"), "utf8");
  expect(ours.messages[0]).toEqual({
    ...autoloaded,
    content: [
      {
        type: "text",
        text: autoloadedOf(text).replace(`Skill: ${AUTOLOADED_PATH}`, `Skill: ${file}`),
      },
    ],
    details: { name: "alpha-ours", path: file },
  });
  expect(ours.injections).toEqual([{ by: "autoload", path: file, at: 0 }]);
  // Every other message is the arm's of the case without the block.
  expect(ours.messages.slice(1)).toEqual(withOurs(f1Case, "ours", RENAMED).messages);
  expect(JSON.stringify(ours.messages)).not.toContain(`Skill: ${AUTOLOADED_PATH}`);
  expect(ours.dispatch).toEqual(dispatchOf(c));
  expect(c.messages[0]).toEqual(autoloaded);

  // dispatch-first against a skill of ours: the plugin's block holds our text, where the plugin
  // puts it.
  const oursDir = path.join(dispatchScratch, "ours");
  mkdirSync(oursDir, { recursive: true });
  writeFileSync(
    path.join(oursDir, "SKILL.md"),
    DISPATCH_FIRST_CUT.replace("name: dispatch-first", "name: dispatch-ours")
  );
  const dispatch = dispatchCase("c-dispatch-ours");
  const built = buildArm(
    dispatch,
    "ours",
    { old: oldDir, ours: { dir: oursDir, name: "dispatch-ours" } },
    path.join(dispatchScratch, "ours-arm")
  );
  expect(built.messages[0]).toEqual({
    role: "user",
    content: [{ type: "text", text: readDispatchFirstContext(path.join(oursDir, "SKILL.md")) }],
    timestamp: 0,
  });
  expect(textOf(built.messages[0])).toContain("Search the made-up tracker before you act.");
  expect(built.injections).toEqual([
    { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 },
  ]);
  expect(built.dispatch).toEqual(dispatchOf(dispatch));
});

// Reads of the cut's own copy of alpha by its path on disk (arms.ts's header). COPY is the copy a
// made-up dev box's session read, SHOWN the directory Oh My Pi's hashline line shows its files
// under (relative to the home directory), and OTHER_COPY another copy of alpha (an eval
// checkout's), which is not the cut's.
const COPY = "/home/agent/src/acme-skills/skills/alpha";
const SHOWN = "~/src/acme-skills/skills/alpha";
const OTHER_COPY = "/tmp/eval/acme-skills/skills/alpha";

/** A `read` call of `target`, alone in an assistant message. */
function readCall(id: string, target: string): SessionMessage {
  return {
    role: "assistant",
    content: [{ type: "toolCall", id, name: "read", arguments: { path: target } }],
  };
}

/** The rows of `text` (a final newline ends its last line) as a session that shows hashline rows
 *  shows them: each line after its number and `:`. */
function numbered(text: string): string {
  return text
    .replace(/\n$/, "")
    .split("\n")
    .map((line, index) => `${index + 1}:${line}`)
    .join("\n");
}

/** Oh My Pi's result for a whole read by path of `file` of COPY whose text is `text` and whose
 *  snapshot tag is `tag`, in a session that shows hashline rows, as the pinned Oh My Pi writes
 *  one: the line `[<path>#<tag>]`, then each line after its number and `:`. */
function pathResult(id: string, file: string, text: string, tag: string): SessionMessage {
  const lines = text.replace(/\n$/, "").split("\n");
  return {
    role: "toolResult",
    toolCallId: id,
    toolName: "read",
    content: [
      {
        type: "text",
        text: `[${SHOWN}/${file}#${tag}]\n${numbered(text)}`,
      },
    ],
    details: {
      totalLines: lines.length,
      rowPrefixes: true,
      displayContent: {
        text: lines.join("\n"),
        startLine: 1,
        lineNumbers: lines.map((_, i) => i + 1),
      },
      fileSize: Buffer.byteLength(text),
      meta: { source: { type: "path", value: `${COPY}/${file}` }, pagedSource: true },
    },
    isError: false,
    timestamp: 1795139990000,
  };
}

/** A read result in a session that shows hashline rows, of `target`, showing `text`. */
function recordedRead(id: string, shown: string, text: string): SessionMessage {
  return {
    role: "toolResult",
    toolCallId: id,
    toolName: "read",
    content: [{ type: "text", text: `[${shown}#BF93]\n${text}` }],
    details: { rowPrefixes: true, meta: { source: { type: "path", value: shown } } },
    isError: false,
  };
}

/** Oh My Pi's not-found result for a read by path of `target`, in the place of `message`. */
function pathNotFound(message: SessionMessage | undefined, target: string): SessionMessage {
  return {
    ...message,
    content: [{ type: "text", text: `Path '${target}' not found` }],
    details: {},
    isError: true,
  };
}

/** F1's case whose session also read alpha's two files whole by path in COPY (indices 8 and 10;
 *  BF93 and B3C8 are Oh My Pi's tags of F1's SKILL.md and references/x.md), the first five lines
 *  of its SKILL.md there (12), and OTHER_COPY's SKILL.md (14). */
const pathMessages: SessionMessage[] = [
  ...messages.slice(0, 7),
  readCall("read_path_skill", `${COPY}/SKILL.md`),
  pathResult("read_path_skill", "SKILL.md", f1File(2), "BF93"),
  readCall("read_path_x", `${COPY}/references/x.md`),
  pathResult("read_path_x", "references/x.md", f1File(4), "B3C8"),
  readCall("read_path_range", `${COPY}/SKILL.md:1-5`),
  recordedRead("read_path_range", `${SHOWN}/SKILL.md`, "1:---\n2:name: alpha"),
  readCall("read_other_copy", `${OTHER_COPY}/SKILL.md`),
  recordedRead("read_other_copy", `${OTHER_COPY}/SKILL.md`, "1:---\n2:name: alpha"),
];
const pathCase: Case = {
  ...f1Case,
  id: "c-path",
  messages: pathMessages,
  skillReads: [2, 4, 8, 10],
  pathCopy: { dir: COPY },
};

/** The arm `name` of `c`, from F1's old directory and `fresh` as the new one. */
function pathArm(c: Case, name: ArmName, fresh = newDir): Arm {
  return buildArm(c, name, { old: oldDir, new: fresh }, path.join(scratch, `path-${c.id}-${name}`));
}

// Catches: an arm that keeps the recorded text of a read of the skill by its path on disk, so
// every arm sends the base text there, or that shows its own directory in it.
test("a whole read by path of the cut's own copy carries each arm's own text at the path the session read, and none answers it as not found", () => {
  // Built from the files the session read, the old arm gives the case's own messages, byte for
  // byte: the form, the tag and the record are Oh My Pi's.
  const old = pathArm(pathCase, "old");
  expect(JSON.stringify(old.messages)).toBe(JSON.stringify(pathCase.messages));
  expect(old.pathCopy).toEqual({ dir: COPY, shownDir: SHOWN, rows: "hashline" });

  const fresh = pathArm(pathCase, "new");
  expect(fresh.messages[8]).toEqual(
    pathResult("read_path_skill", "SKILL.md", newSkill, snapshotTag(newSkill))
  );
  expect(snapshotTag(newSkill)).not.toBe("BF93");
  // The new text has no references/x.md: Oh My Pi's not-found result for the path read.
  expect(fresh.messages[10]).toEqual(pathNotFound(pathMessages[10], `${COPY}/references/x.md`));
  expect(JSON.stringify(fresh.messages)).not.toContain(newDir);
  // A new read, as replay asks one, is answered the same way.
  expect(pathFile(fresh, `${COPY}/SKILL.md`)).toEqual({
    text: textOf(fresh.messages[8]),
    isError: false,
  });
  expect(pathFile(fresh, `${COPY}/references/x.md`)).toEqual({
    text: `Path '${COPY}/references/x.md' not found`,
    isError: true,
  });

  const minus = pathArm(pathCase, "minus");
  const less = f1File(2).replace(`${LINE}\n`, "");
  expect(minus.messages[8]).toEqual(
    pathResult("read_path_skill", "SKILL.md", less, snapshotTag(less))
  );

  // none keeps each read call by path, its result Oh My Pi's not-found one; its skill:// reads go.
  const none = pathArm(pathCase, "none");
  expect(none.messages).toContainEqual(pathNotFound(pathMessages[8], `${COPY}/SKILL.md`));
  expect(none.messages).toContainEqual(pathNotFound(pathMessages[10], `${COPY}/references/x.md`));
  expect(none.messages).toContainEqual(pathMessages[7]);
  expect(none.messages).not.toContainEqual(pathMessages[8]);
  expect(none.messages.filter((message) => readIds.includes(message.toolCallId))).toEqual([]);
  expect(pathFile(none, `${COPY}/SKILL.md`)).toEqual({
    text: `Path '${COPY}/SKILL.md' not found`,
    isError: true,
  });
});

// Catches: an arm that rewrites a read the cut's copy does not answer: one with a selector other
// than one line range, one of another copy, or one of a case that names no copy.
test("a read by path with a selector other than one line range, one of another copy, and one of a case with no pathCopy stay as recorded", () => {
  const { pathCopy: _, ...noCopy } = { ...pathCase, id: "c-no-copy", skillReads: [2, 4] };
  for (const name of ["old", "new", "minus", "twin", "none"] as const) {
    const each = pathArm(pathCase, name);
    for (const index of [11, 12, 13, 14]) {
      expect(each.messages).toContainEqual(pathMessages[index]);
    }
    expect(pathFile(each, `${COPY}/SKILL.md:raw`)).toBeUndefined();
    expect(pathFile(each, `${OTHER_COPY}/SKILL.md`)).toBeUndefined();
    const bare = pathArm(noCopy, name);
    // Its copy is the directory its skill:// reads show alpha installed in, which COPY is not under.
    expect(bare.pathCopy).toEqual({ dir: INSTALLED, shownDir: INSTALLED, rows: "hashline" });
    for (const index of [7, 8, 9, 10, 11, 12, 13, 14]) {
      expect(bare.messages).toContainEqual(pathMessages[index]);
    }
    expect(pathFile(bare, `${COPY}/SKILL.md`)).toBeUndefined();
  }
});

// Catches: the arm of ours keeping their text in a whole read by path of the cut's own copy, or
// answering a read of a file ours lacks where it removes the skill:// read of that file.
test("in the arm of ours, a whole read by path of the cut's own copy carries our file at the same path, and goes with its call when ours lacks the file", () => {
  // alpha-ours: our SKILL.md at the path the session read, and references/x.md as recorded.
  const renamed = withOurs(pathCase, "ours", RENAMED);
  const oursSkill = readFileSync(path.join(renamedDir, "SKILL.md"), "utf8");
  expect(renamed.pathCopy).toEqual({ dir: COPY, shownDir: SHOWN, rows: "hashline" });
  expect(renamed.messages[7]).toEqual(pathMessages[7]);
  expect(renamed.messages[8]).toEqual(
    pathResult("read_path_skill", "SKILL.md", oursSkill, snapshotTag(oursSkill))
  );
  expect(renamed.messages.slice(9)).toEqual(pathMessages.slice(9));
  expect(renamed.ours?.removedReads).toEqual([]);
  expect(JSON.stringify(renamed.messages)).not.toContain(renamedDir);

  // alpha-short has no references/x.md: its skill:// read and its read by path go with their
  // calls, and the arm records both, in the history's order.
  const short = withOurs(pathCase, "ours", SHORT);
  expect(short.ours?.removedReads).toEqual(["references/x.md", "references/x.md"]);
  expect(short.messages).toContainEqual(pathMessages[7]);
  expect(short.messages).toContainEqual(
    pathResult("read_path_skill", "SKILL.md", SHORT_SKILL, snapshotTag(SHORT_SKILL))
  );
  const ids = short.messages.flatMap((message) => [
    message.toolCallId,
    ...(message.content ?? []).map((part) => part.id),
  ]);
  expect(ids).not.toContain("read_path_x");
  expect(short.messages.slice(-4)).toEqual(pathMessages.slice(11));

  // A new read, as replay asks one: our file at the path read, or Oh My Pi's not-found result for
  // a file ours lacks.
  expect(pathFile(renamed, `${COPY}/SKILL.md`)).toEqual({
    text: textOf(renamed.messages[8]),
    isError: false,
  });
  expect(pathFile(short, `${COPY}/references/x.md`)).toEqual({
    text: `Path '${COPY}/references/x.md' not found`,
    isError: true,
  });
});

// Catches: a read of the copy by a path relative to the session's working directory, or under
// `~`, that an arm leaves as recorded or answers at its absolute path, or a not-found result that
// names another path than the one the session read.
test("a whole read of the copy relative to the session's working directory, or under ~, is answered as a read by its absolute path is, at the path the session read, and none answers it as not found", () => {
  const relative = "skills/alpha/SKILL.md";
  const tilde = "~/src/acme-skills/skills/alpha/references/x.md";
  const copy = { dir: COPY, cwd: "/home/agent/src/acme-skills", home: "/home/agent" };
  const resolved: Case = {
    ...pathCase,
    id: "c-path-resolved",
    messages: pathMessages.map((message, index) =>
      index === 7
        ? readCall("read_path_skill", relative)
        : index === 9
          ? readCall("read_path_x", tilde)
          : message
    ),
    pathCopy: copy,
  };
  // Built from the files the session read, the old arm gives the case's own messages, byte for
  // byte: the record names the absolute path each read opened.
  const old = pathArm(resolved, "old");
  expect(JSON.stringify(old.messages)).toBe(JSON.stringify(resolved.messages));
  expect(old.pathCopy).toEqual({ ...copy, shownDir: SHOWN, rows: "hashline" });

  const fresh = pathArm(resolved, "new");
  expect(fresh.messages[8]).toEqual(
    pathResult("read_path_skill", "SKILL.md", newSkill, snapshotTag(newSkill))
  );
  expect(fresh.messages[10]).toEqual(pathNotFound(pathMessages[10], tilde));
  expect(pathFile(fresh, relative)).toEqual({ text: textOf(fresh.messages[8]), isError: false });
  expect(pathFile(fresh, `./${relative}`)).toEqual({
    text: textOf(fresh.messages[8]),
    isError: false,
  });

  const none = pathArm(resolved, "none");
  expect(none.messages).toContainEqual(pathNotFound(pathMessages[8], relative));
  expect(none.messages).toContainEqual(pathNotFound(pathMessages[10], tilde));

  // A copy that names no working directory does not take the relative read.
  const { cwd: _, ...noCwd } = copy;
  const unresolved = pathArm({ ...resolved, pathCopy: noCwd, skillReads: [2, 4, 10] }, "new");
  expect(unresolved.messages[8]).toEqual(resolved.messages[8]);
  expect(pathFile(unresolved, relative)).toBeUndefined();
});

// Catches: a case whose session read its skill by skill:// only, whose arms leave a read by path of
// a file under the installed directory its `[Skill file: …]` line shows to the step (the defect:
// ce-simplify-code's session cases judged on such reads, using-subagents' written cases left with no
// verdict), or answer it with one text in every arm.
natives.test(
  "a case that read its skill by skill:// only answers a read by path of a file under the installed directory its header shows as a read of the cut's own copy, and none answers it as not found",
  () => {
    const x = `${INSTALLED}/references/x.md`;
    const copy = { dir: INSTALLED, shownDir: INSTALLED, rows: "hashline" as const };
    // f1Case names no pathCopy, and its reads of alpha are skill:// reads under INSTALLED. Its
    // history is as before: BF93 and B3C8 are Oh My Pi's tags of F1's SKILL.md and references/x.md.
    const old = arm("old");
    expect(old.pathCopy).toEqual(copy);
    expect(JSON.stringify(old.messages)).toBe(JSON.stringify(f1Case.messages));
    expect(pathFile(old, x)).toEqual({
      text: `[${x}#B3C8]\n${numbered(f1File(4))}`,
      isError: false,
    });
    expect(pathFile(old, `${INSTALLED}/SKILL.md:1-5`)?.text).toStartWith(
      `[${INSTALLED}/SKILL.md#BF93]\n1:---\n2:name: alpha\n`
    );
    // A directory under it is not answered: Oh My Pi lists it, which the arms do not rebuild.
    expect(pathFile(old, `${INSTALLED}/references`)).toBeUndefined();
    const fresh = arm("new");
    expect(pathFile(fresh, `${INSTALLED}/SKILL.md`)).toEqual({
      text: `[${INSTALLED}/SKILL.md#${snapshotTag(newSkill)}]\n${numbered(newSkill)}`,
      isError: false,
    });
    expect(pathFile(fresh, x)).toEqual({ text: `Path '${x}' not found`, isError: true });
    const none = arm("none");
    expect(none.pathCopy).toEqual(copy);
    expect(pathFile(none, x)).toEqual({ text: `Path '${x}' not found`, isError: true });
    // The arm of ours answers it as a read of the cut's own copy: our file at that path.
    const ours = withOurs(f1Case, "ours", RENAMED);
    expect(ours.pathCopy).toEqual(copy);
    expect(pathFile(ours, x)).toEqual(pathFile(old, x));
    expect(pathFile(withOurs(f1Case, "ours", SHORT), x)).toEqual({
      text: `Path '${x}' not found`,
      isError: true,
    });

    // A case whose reads show no installed directory names none.
    const bare = structuredClone(f1Case);
    for (const index of bare.skillReads) {
      const part = bare.messages[index]?.content?.[0];
      if (part) part.text = stripSkillHeader(part.text ?? "");
    }
    const plain = buildArm(
      bare,
      "old",
      { old: oldDir, new: newDir },
      path.join(scratch, "bare-copy")
    );
    expect(plain.pathCopy).toBeUndefined();
    expect(pathFile(plain, "/skills/alpha/references/x.md")).toBeUndefined();
  }
);

// Catches: a long line of an arm's file shown whole where Oh My Pi cuts it, and a file Oh My Pi
// shows in another form answered as if it showed it whole.
test("a line past 768 characters is cut as Oh My Pi cuts it, with its notice, and a file Oh My Pi shows in a form the arms do not rebuild is refused, naming the case", () => {
  const long = `${"x".repeat(799)}.`;
  const last = "3. Stop after the third step.";
  const wide = f1File(2).replace(LINE, `${LINE} ${long}`).replace(last, `${last} ${long}`);
  const wideDir = skillDir("wide", { "SKILL.md": wide });
  const wideArm = pathArm(pathCase, "new", wideDir);
  const answer = textOf(wideArm.messages[8]);
  const rows = answer.split("\n");
  expect(rows[9]).toBe(`9:${`${LINE} ${long}`.slice(0, 768)}…`);
  expect(rows[10]).toBe(`10:${`${last} ${long}`.slice(0, 768)}…`);
  expect(answer).toEndWith(
    "\n\n[Some lines truncated to 768 chars. Use :raw:9-9 to read line 9 whole; cut lines run to line 10, each read the same way]"
  );
  expect(detailsOf(wideArm.messages[8]).meta).toEqual({
    source: { type: "path", value: `${COPY}/SKILL.md` },
    pagedSource: true,
    limits: {
      columnTruncated: {
        maxColumn: 768,
        unit: "chars",
        lines: { first: 9, last: 10 },
        selectorBase: "",
      },
    },
  });

  const tall = `${f1File(2)}${"More.\n".repeat(291)}`;
  const tallDir = skillDir("tall", { "SKILL.md": tall });
  expect(() => pathArm(pathCase, "new", tallDir)).toThrow(
    `case c-path: the new arm cannot answer the read of ${COPY}/SKILL.md: SKILL.md has 301 lines, and a read by path shows 300, then the end of each block past them, a form the arms do not rebuild`
  );
  const tallArm: Arm = { ...pathArm(pathCase, "old"), skillDir: tallDir };
  expect(pathFile(tallArm, `${COPY}/SKILL.md`)).toBeUndefined();
});

// Catches: a read answered with hashline rows in a session whose reads show the lines alone.
test("a read whose result shows the lines alone is answered with the lines alone, and snapshotTag is Oh My Pi's", () => {
  const plain = structuredClone(pathCase);
  plain.id = "c-plain";
  const result = plain.messages[8];
  if (result === undefined) throw new Error("no result at 8");
  result.content = [{ type: "text", text: f1File(2).replace(/\n$/, "") }];
  result.details = { ...detailsOf(result), rowPrefixes: false };
  const fresh = pathArm(plain, "new");
  expect(textOf(fresh.messages[8])).toBe(newSkill.replace(/\n$/, ""));
  expect(detailsOf(fresh.messages[8]).rowPrefixes).toBe(false);
  // The tags of crates/pi-edit/src/store.rs's own test (lines 485-489 at the pin).
  expect(snapshotTag("a \n b\t\r\nc")).toBe("80BA");
  expect(snapshotTag("hello\n")).toBe("5BF9");
  expect(snapshotTag("")).toBe("5D05");
});

// Catches: a case builder that cannot tell, before it writes a case, which files buildArm refuses
// to answer by path, so a behaviour run fails on one.
test("pathFormFault names each form of a file the arms do not rebuild, as buildArm refuses it, and none for a file they rebuild", () => {
  expect(pathFormFault("SKILL.md", Buffer.from(f1File(2)))).toBeUndefined();
  // A view into a larger buffer reads its own bytes only.
  expect(pathFormFault("SKILL.md", Buffer.from(`\0\0${f1File(2)}`).subarray(2))).toBeUndefined();
  expect(pathFormFault("notes.txt", Buffer.from("One note.\n"))).toBeUndefined();
  // A line Oh My Pi cuts at 768 characters is a form the arms rebuild.
  expect(pathFormFault("SKILL.md", Buffer.from(`${"x".repeat(800)}\n`))).toBeUndefined();
  expect(pathFormFault("SKILL.md", Buffer.from("A line.\n".repeat(300)))).toBeUndefined();

  expect(pathFormFault("scripts/run.py", Buffer.from("print(1)\n"))).toBe(
    "scripts/run.py is no Markdown or plain-text file, the only kinds whose read by path the arms rebuild"
  );
  expect(pathFormFault("SKILL.md", Uint8Array.of(0x61, 0x00, 0x62))).toBe(
    "SKILL.md is not UTF-8 text"
  );
  expect(pathFormFault("SKILL.md", Uint8Array.of(0x61, 0xff, 0x62))).toBe(
    "SKILL.md is not UTF-8 text"
  );
  expect(pathFormFault("SKILL.md", Buffer.from("A line.\n".repeat(301)))).toBe(
    "SKILL.md has 301 lines, and a read by path shows 300, then the end of each block past them"
  );
  const merging = Buffer.from("# Merging\n\n<<<<<<< ours\n");
  expect(pathFormFault("SKILL.md", merging)).toBe(
    "line 3 of SKILL.md opens a git conflict block, which a read by path follows with a warning"
  );
  // A file past 4 MiB the pin streams, with a notice of its own.
  expect(pathFormFault("SKILL.md", Buffer.alloc(4 * 1024 * 1024 + 1, "a"))).toBe(
    "SKILL.md is past the 4.0MB a read holds whole, so the pin streams it, with a notice the runner does not rebuild"
  );

  // buildArm refuses the arm's file with the fault pathFormFault names.
  const mergeDir = skillDir("merging", { "SKILL.md": merging.toString() });
  expect(() => pathArm(pathCase, "new", mergeDir)).toThrow(
    `case c-path: the new arm cannot answer the read of ${COPY}/SKILL.md: ${pathFormFault("SKILL.md", merging)}, a form the arms do not rebuild`
  );
});

// Catches: a page the byte budget stops answered with its rows alone, where the pin shows the
// closing line of the block the page cuts after them, outside the budget.
natives.test(
  "a whole read by path the byte budget stops shows the closing line of the block it cuts after its rows",
  () => {
    // Numbered pages stop at 50,000 bytes. Plain pages have room for all 100 lines. The 100 lines
    // are one Markdown paragraph, which opens on line 1 and closes on line 100, past the page.
    const line = "y".repeat(500);
    const wide = `${line}\n`.repeat(100);
    expect(pathFormFault("SKILL.md", Buffer.from(wide))).toBeUndefined();
    const wideArm = pathArm(pathCase, "new", skillDir("byte-page", { "SKILL.md": wide }));
    const rows = Array.from({ length: 100 }, (_, i) => `${i + 1}:${line}`).join("\n");
    expect(textOf(wideArm.messages[8])).toBe(
      `[${SHOWN}/SKILL.md#${snapshotTag(wide)}]\n${rows}\n\n[Showing lines 1-99 of 100 (48.8KB limit). Use :100 to continue]`
    );
    expect(detailsOf(wideArm.messages[8]).displayContent).toEqual({
      text: wide.replace(/\n$/, ""),
      startLine: 1,
      lineNumbers: Array.from({ length: 100 }, (_, i) => i + 1),
    });
  }
);

// Catches: a summary's record unlike the pin's (R4's recorded summaries hold `displayContent`,
// the rows' lines alone from line 1, `rowPrefixes`, `summary` with its rows, elided spans and
// elided lines, `fileSize` and `meta.source`, in that order, and no page's `pagedSource`), or a
// code file of a skill's own read by path summarized, where the arms rebuild no such read.
natives.test(
  "a checkout's whole read by path of a code file the pin summarizes gives the pin's summary and its record; a skill's own code file is still no form the arms rebuild",
  () => {
    const table = `${[
      "TABLE = {",
      ...Array.from({ length: 118 }, (_, n) => `    "k${n + 1}": ${n + 1},`),
      "}",
    ].join("\n")}\n`;
    const dir = skillDir("summary", { "src/table.py": table });
    const source = "/work/acme/proj/src/table.py";
    const read = {
      file: "src/table.py",
      target: "src/table.py",
      source,
      shown: "src/table.py",
      rows: "hashline" as const,
    };
    const answer = pathAnswer(dir, { ...read, checkout: { listed: "src/table.py" } });
    expect(answer).toEqual({
      text: `[src/table.py#${snapshotTag(table)}]\n1-120:TABLE = { … }\n\n[…118ln elided; re-read needed ranges with src/table.py:1-120]`,
      isError: false,
      details: {
        displayContent: { text: "TABLE = { … }", startLine: 1 },
        rowPrefixes: true,
        summary: { lines: 1, elidedSpans: 1, elidedLines: 118 },
        fileSize: Buffer.byteLength(table),
        meta: { source: { type: "path", value: source } },
      },
    });
    if (typeof answer === "string") throw new Error(answer);
    expect(Object.keys(answer.details)).toEqual([
      "displayContent",
      "rowPrefixes",
      "summary",
      "fileSize",
      "meta",
    ]);
    expect(pathAnswer(dir, read)).toBe(
      "src/table.py is no Markdown or plain-text file, the only kinds whose read by path the arms rebuild"
    );
  }
);

/** Beta's SKILL.md and its reference, as the cache holds the copy the capture installed. */
const BETA_MD =
  "---\nname: beta\ndescription: Use before any step of the made-up fixture project.\n---\n\n# Beta\n\nLoad alpha first.\n";
const BETA_R = "# Beta reference\n\nOne line.\n";

/** Beta's installed copy, a cache entry as context.ts's installedCopies names it. */
function betaCopies(): InstalledCopy[] {
  const dir = skillDir("cache-beta", { "SKILL.md": BETA_MD, "references/r.md": BETA_R });
  return [{ name: "beta", dir }];
}

/** F1's case whose session also read beta's SKILL.md (7, 8), which Oh My Pi showed at
 *  /opt/plugin/skills/beta. */
const betaCase: Case = {
  ...f1Case,
  id: "c-beta",
  messages: [
    ...messages.slice(0, 7),
    readCall("read_beta", "skill://beta"),
    {
      role: "toolResult",
      toolCallId: "read_beta",
      toolName: "read",
      content: [
        { type: "text", text: `[Skill file: /opt/plugin/skills/beta/SKILL.md]\n${BETA_MD.trim()}` },
      ],
      isError: false,
    },
  ],
};

// Catches: a read of another skill answered from an arm's own text, at a path that differs
// between arms, or a read of a skill the arm does not install answered other than Oh My Pi does.
test("a skill:// read of another skill of the profile is answered from its installed copy, the same in every arm, at the directory the case's reads of it show; one of a skill the profile does not list gets Oh My Pi's unknown-skill text", () => {
  const copies = betaCopies();
  const answer = (dir: string, file: string, text: string) => ({
    skill: "beta",
    file,
    by: "skill" as const,
    from: "copy" as const,
    text: `[Skill file: ${dir}/${file}]\n${text.replace(/\n$/, "")}`,
    isError: false,
  });
  const beside = path.join(path.dirname(INSTALLED), "beta");
  for (const name of ["old", "new", "none", "minus", "twin"] as const) {
    const dirs = { old: oldDir, new: newDir };
    const shown = buildArm(betaCase, name, dirs, path.join(scratch, `beta-${name}`));
    expect(shown.skillDirs).toEqual({ beta: "/opt/plugin/skills/beta" });
    expect(otherSkillRead(shown, copies, "skill://beta")).toEqual(
      answer("/opt/plugin/skills/beta", "SKILL.md", BETA_MD)
    );
    // A case whose reads show beta at no directory: beside the case's skill's installed one.
    expect(arm(name).skillDirs).toEqual({});
    expect(otherSkillRead(arm(name), copies, "skill://beta/SKILL.md")).toEqual(
      answer(beside, "SKILL.md", BETA_MD)
    );
  }
  // The arm of ours answers a read of beta as every other arm does.
  const ours = withOurs(betaCase, "ours", RENAMED);
  expect(ours.skillDirs).toEqual({ beta: "/opt/plugin/skills/beta" });
  expect(otherSkillRead(ours, copies, "skill://beta")).toEqual(
    answer("/opt/plugin/skills/beta", "SKILL.md", BETA_MD)
  );
  expect(otherSkillRead(arm("old"), copies, "skill://beta/references/r.md")).toEqual(
    answer(beside, "references/r.md", BETA_R)
  );
  expect(otherSkillRead(arm("old"), copies, "skill://beta/references/gone.md")).toEqual({
    ...answer(beside, "references/gone.md", ""),
    text: `File not found: ${beside}/references/gone.md`,
    isError: true,
  });
  // A skill the profile does not list: F1's own answer to skill://gamma.
  expect(otherSkillRead(arm("old"), copies, "skill://gamma")).toEqual({
    skill: "gamma",
    file: "SKILL.md",
    by: "skill",
    from: "absent",
    text: textOf(messages[6]),
    isError: true,
  });
  expect(otherSkillRead(arm("none"), copies, "skill://gamma/references/x.md")).toEqual({
    skill: "gamma",
    file: "references/x.md",
    by: "skill",
    from: "absent",
    text: "Unknown skill: gamma\nAvailable: beta",
    isError: true,
  });
  // No answer for a read of the case's skill, one with a selector other than one line range, one
  // of a directory, another URL, or a path no rule names a skill by; one line range is answered.
  for (const target of [
    "skill://alpha",
    "skill://beta/SKILL.md:raw",
    "skill://beta/references",
    "https://example.test/skills/beta/SKILL.md",
    "/home/agent/notes/beta.md",
  ]) {
    expect(otherSkillRead(arm("old"), copies, target)).toBeUndefined();
  }
  expect(otherSkillRead(arm("old"), copies, "skill://beta/SKILL.md:1-5")).toMatchObject({
    skill: "beta",
    file: "SKILL.md",
    from: "copy",
    isError: false,
  });
  // With no installed copy of beta, a read of it fails rather than be answered as not found.
  expect(() => otherSkillRead(arm("old"), [], "skill://beta")).toThrow(
    "the old arm's profile installs beta, and the replay holds no installed copy of it"
  );
});

// Catches: a copy of the case's skill's source under another name answered from no arm, or from
// one copy for every arm.
test("a skill:// read of a copy of the case's skill's source under a namespace is answered from the arm's directory, and none answers it as Oh My Pi answers a skill it lacks", () => {
  const twice: Case = {
    ...f1Case,
    id: "c-twice",
    profile: {
      ...f1Case.profile,
      skills: [
        ...f1Case.profile.skills,
        {
          repo: "acme/skills",
          commit: "fedcba9876543210",
          path: "skills/alpha",
          name: "alpha",
          namespace: "ns",
        },
      ],
    },
  };
  const dirs = { old: oldDir, new: newDir };
  const fresh = buildArm(twice, "new", dirs, path.join(scratch, "twice-new"));
  expect(otherSkillRead(fresh, [], "skill://ns/alpha")).toEqual({
    skill: "ns/alpha",
    file: "SKILL.md",
    by: "skill",
    from: "arm",
    text: `[Skill file: ${path.join(path.dirname(INSTALLED), "ns/alpha")}/SKILL.md]\n${newSkill.replace(/\n$/, "")}`,
    isError: false,
  });
  // Oh My Pi names the URL's host when neither `ns/alpha` nor `ns` is a skill it has.
  const none = buildArm(twice, "none", dirs, path.join(scratch, "twice-none"));
  expect(otherSkillRead(none, [], "skill://ns/alpha")).toMatchObject({
    skill: "ns",
    from: "absent",
    text: "Unknown skill: ns\nAvailable: beta",
  });
});

// Catches: a whole read by path of another skill's file taken for no skill read, answered at a
// path the session does not show, or answered where the copy may not be the file the session
// would open.
test("a whole read by path of another skill of the profile, by count.sql's rule, is answered from its copy in Oh My Pi's form at the path the session shows; a name two skills share, the case's skill's, and a file the copy lacks are not answered", () => {
  const copies = betaCopies();
  const skillMd = (shown: string) => `[${shown}#${snapshotTag(BETA_MD)}]\n${numbered(BETA_MD)}`;
  const target = "/home/agent/src/acme-skills/skills/beta/SKILL.md";
  const read = {
    skill: "beta",
    file: "SKILL.md",
    by: "path",
    from: "copy",
    isError: false,
  } as const;
  const old = arm("old");
  expect(old.pathRows).toBe("hashline");
  for (const each of [old, arm("none")]) {
    expect(otherSkillRead(each, copies, target)).toEqual({ ...read, text: skillMd(target) });
  }
  // A session that names its working directory and home shows a file under the first relative to
  // it, and one under the second after `~`.
  const placed: Arm = {
    ...old,
    pathCopy: {
      dir: COPY,
      cwd: "/home/agent/src/acme-skills",
      home: "/home/agent",
      shownDir: SHOWN,
      rows: "hashline",
    },
  };
  expect(otherSkillRead(placed, copies, target)?.text).toBe(skillMd("skills/beta/SKILL.md"));
  expect(otherSkillRead(placed, copies, "./skills/beta/SKILL.md")?.text).toBe(
    skillMd("skills/beta/SKILL.md")
  );
  expect(otherSkillRead(placed, copies, "~/plugins/beta/SKILL.md")?.text).toBe(
    skillMd("~/plugins/beta/SKILL.md")
  );
  const reference = "/home/agent/plugins/skills/beta/references/r.md";
  expect(otherSkillRead(placed, copies, reference)).toEqual({
    ...read,
    file: "references/r.md",
    text: `[~/plugins/skills/beta/references/r.md#${snapshotTag(BETA_R)}]\n${numbered(BETA_R)}`,
  });
  // A session whose recorded tools leave out the edit tool shows the lines alone.
  const noEdit: Case = {
    ...f1Case,
    id: "c-no-edit",
    profile: { ...f1Case.profile, tools: ["read"] },
  };
  const plain = buildArm(
    noEdit,
    "old",
    { old: oldDir, new: newDir },
    path.join(scratch, "no-edit")
  );
  expect(plain.pathRows).toBe("plain");
  expect(otherSkillRead(plain, copies, target)?.text).toBe(BETA_MD.replace(/\n$/, ""));

  // No answer for a file the copy lacks, a file outside the rule, the case's skill, a selector, a
  // directory, or a name two skills of the profile share.
  for (const each of [
    "/home/agent/src/acme-skills/skills/beta/references/gone.md",
    "/home/agent/src/acme-skills/skills/beta/notes/n.md",
    "/home/agent/src/acme-skills/skills/alpha/SKILL.md",
    `${target}:1-5`,
    "/home/agent/src/acme-skills/skills/beta/",
  ]) {
    expect(otherSkillRead(old, copies, each)).toBeUndefined();
  }
  const shared: Case = {
    ...f1Case,
    id: "c-shared",
    profile: {
      ...f1Case.profile,
      skills: [
        ...f1Case.profile.skills,
        {
          repo: "acme/other",
          commit: "0123456789abcdef",
          path: "beta",
          name: "beta",
          namespace: "ns",
        },
      ],
    },
  };
  const both = buildArm(shared, "old", { old: oldDir, new: newDir }, path.join(scratch, "shared"));
  expect(otherSkillRead(both, copies, target)).toBeUndefined();
});

// Catches: the autoloaded block's body read from a SKILL.md that starts with a byte order mark
// with the mark kept, so its frontmatter was not found and went into the block whole, where Oh
// My Pi drops the mark as it reads the file and puts in the body alone.
test("the autoloaded block of a SKILL.md that starts with a byte order mark holds its body alone, as Oh My Pi reads it", () => {
  const dir = path.join(scratch, "autoload-mark");
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    path.join(dir, "SKILL.md"),
    "\uFEFF---\nname: alpha\ndescription: Use for the alpha list.\n---\n\n# Alpha\n\nStep one.\n"
  );
  expect(autoloadedText(dir, "/skills/alpha/SKILL.md")).toBe(
    "# Alpha\n\nStep one.\n\n---\n\nSkill: /skills/alpha/SKILL.md"
  );
});
