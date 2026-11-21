import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SessionEntry, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import { type Case, loadCases, pathHash, skillReadFile } from "./cases";

// The loader of the case file, on F1 (testdata/F1): an Oh My Pi 18.18.6 session of a scratch
// profile with the made-up skills alpha and beta, captured with
// `HOME=<scratch> omp -p --session-dir <scratch>/sessions --model anthropic/claude-sonnet-5-5
// "<prompt>"` (testdata/CAPTURE.md). Its messages are: 0 the user's prompt, 1 a read of
// skill://alpha, 2 its result, 3 a read of skill://alpha/references/x.md, 4 its result, 5 a read
// of the missing skill://gamma, 6 its not-found result, 7 a `task` call (the step here), and the
// rest of the session.

const F1 = path.join(
  import.meta.dir,
  "testdata",
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
const scratch = temporaryDirectory("skill-cuts-cases", { real: true });

/** F1's messages, in order. */
function f1Messages(): SessionMessage[] {
  return readFileSync(F1, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => SessionEntry.parse(JSON.parse(line)))
    .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));
}

/** The text of F1's read result at `index`, less its header, with the newline the read drops. */
function readText(messages: SessionMessage[], index: number): string {
  return `${stripSkillHeader(messages[index]?.content?.[0]?.text ?? "")}\n`;
}

/** A skill directory under the scratch directory holding the alpha files F1 read. */
function alphaDir(name: string): string {
  const messages = f1Messages();
  const dir = path.join(scratch, name);
  mkdirSync(path.join(dir, "references"), { recursive: true });
  writeFileSync(path.join(dir, "SKILL.md"), readText(messages, 2));
  writeFileSync(path.join(dir, "references", "x.md"), readText(messages, 4));
  return dir;
}

/** A case of F1's `task` step, which line 9 of alpha's SKILL.md governs. */
function f1Case(id = "c1"): Case {
  const messages = f1Messages();
  return {
    id,
    cut: "acme-skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: "2. Write each step's result on one line.", lineNumber: 9, lineCount: 10 },
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
      contextFiles: [{ repo: "acme/project", commit: "fedcba9876543210", path: "AGENTS.md" }],
    },
    messages: messages.slice(0, 7),
    skillReads: [2, 4],
    realStep: messages[7] ?? {},
    violatingStep: {
      role: "assistant",
      content: [
        { type: "text", text: "Step 4." },
        { type: "toolCall", id: "violating_1", name: "read", arguments: { path: "skill://beta" } },
      ],
    },
    source: { store: "store_a", pathHash: pathHash("/sessions/alpha.jsonl") },
    sessionStart: "2026-11-20T01:59:41.780Z",
    secretsRemoved: 0,
    sessionDigest: "46874d907e557998",
  };
}

/** A case file under the scratch directory holding `rows`, one JSON line each. */
function caseFile(name: string, rows: unknown[]): string {
  const file = path.join(scratch, `${name}.jsonl`);
  writeFileSync(file, rows.map((row) => `${JSON.stringify(row)}\n`).join(""));
  return file;
}

test("a case of F1 loads", () => {
  const base = alphaDir("base-loads");
  const cases = loadCases(caseFile("loads", [f1Case()]), base);
  expect(cases).toEqual([f1Case()]);
  expect(cases[0]?.messages[2]?.content?.[0]?.text).toStartWith("[Skill file: /");
});

test("a case file is read line by line: a case longer than one read, and the line numbers after it", () => {
  const base = alphaDir("base-long");
  // A user message of more than 2 MiB, of characters of 2, 3 and 4 bytes, so a line spans reads.
  const long = f1Case("c-long");
  const words = "é ✓ 𝄞 ".repeat(Math.ceil((5 * 2 ** 19) / 14));
  long.messages = [{ role: "user", content: [{ type: "text", text: words }] }, ...long.messages];
  long.skillReads = long.skillReads.map((index) => index + 1);
  const file = path.join(scratch, "long.jsonl");
  writeFileSync(file, `${JSON.stringify(long)}\n\nnot json\n${JSON.stringify(f1Case("c2"))}`);
  expect(() => loadCases(file, base)).toThrow(`refused cases:\n${file}:3 is not JSON: `);
  writeFileSync(file, `${JSON.stringify(long)}\r\n\n${JSON.stringify(f1Case("c2"))}`);
  const cases = loadCases(file, base);
  expect(cases.map((c) => c.id)).toEqual(["c-long", "c2"]);
  expect(cases[0]?.messages[0]?.content?.[0]?.text).toBe(words);
});

test("a case with the builder's evidence loads, and an evidence entry with no source is refused", () => {
  const base = alphaDir("base-evidence");
  const evidence = [
    { item: "role", source: "no role: the context is devbox" },
    { item: "skill alpha", source: "[Skill file: …/skills/alpha/SKILL.md] at message 2" },
  ];
  const withEvidence = { ...f1Case("c-evidence"), evidence };
  expect(loadCases(caseFile("evidence", [withEvidence]), base)).toEqual([withEvidence]);
  const blank = { ...f1Case("c-blank"), evidence: [{ item: "role", source: "" }] };
  const file = caseFile("evidence-blank", [blank]);
  expect(() => loadCases(file, base)).toThrow(
    `case c-blank (${file}:1) is not a case: evidence.0.source`
  );
});

test("a subagent's case, with its recorded system prompt and tools, loads; a context of another shape is refused", () => {
  const base = alphaDir("base-subagent");
  const c1 = f1Case("c-subagent");
  const subagent = {
    ...c1,
    context: "subagent:task",
    profile: {
      ...c1.profile,
      contextFiles: [],
      systemPrompt: ["A made-up prompt.\n<skills>\n- alpha: Use for alpha.\n</skills>\n"],
      tools: ["read", "bash"],
    },
  };
  expect(loadCases(caseFile("subagent", [subagent]), base)).toEqual([subagent]);
  const odd = { ...f1Case("c-odd"), context: "subagent:" };
  const noTools = { ...subagent, id: "c-no-tools", profile: { ...subagent.profile, tools: [] } };
  const file = caseFile("subagent-odd", [odd, noTools]);
  expect(() => loadCases(file, base)).toThrow(`case c-odd (${file}:1) is not a case: context`);
  expect(() => loadCases(file, base)).toThrow(
    `case c-no-tools (${file}:2) is not a case: profile.tools`
  );
});

test("refuses a line that is not a whole line of the base text", () => {
  const base = alphaDir("base-line");
  const part = {
    ...f1Case("c-part"),
    line: { ...f1Case().line, text: "Write each step's result" },
  };
  const elsewhere = { ...f1Case("c-elsewhere"), line: { ...f1Case().line, lineNumber: 8 } };
  // The whole line with its spaces trimmed is the line.
  const padded = {
    ...f1Case("c-padded"),
    line: { ...f1Case().line, text: "  2. Write each step's result on one line. " },
  };
  const file = caseFile("line", [part, elsewhere]);
  expect(() => loadCases(file, base)).toThrow(
    `case c-part (${file}:1): its line is not a whole line of ${path.join(base, "SKILL.md")}: "Write each step's result"`
  );
  expect(() => loadCases(file, base)).toThrow(
    `case c-elsewhere (${file}:2): line 8 of ${path.join(base, "SKILL.md")} is not its line`
  );
  expect(loadCases(caseFile("padded", [padded]), base)).toHaveLength(1);
});

test("refuses a case with no realStep, naming it", () => {
  const base = alphaDir("base-real-step");
  const { realStep: _, ...noStep } = f1Case("c-no-step");
  const file = caseFile("no-step", [noStep]);
  expect(() => loadCases(file, base)).toThrow(
    `case c-no-step (${file}:1) has no realStep (the real agent's next assistant message)`
  );
  const userStep = { ...f1Case("c-user-step"), realStep: f1Messages()[0] };
  const userFile = caseFile("user-step", [userStep]);
  expect(() => loadCases(userFile, base)).toThrow(
    `case c-user-step (${userFile}:1) is not a case: realStep: is not an assistant message`
  );
});

test("refuses a case with no violatingStep, and one that is no assistant step or holds nothing", () => {
  const base = alphaDir("base-violating-step");
  const { violatingStep: _, ...noViolating } = f1Case("c-no-violating");
  const file = caseFile("no-violating", [noViolating]);
  expect(() => loadCases(file, base)).toThrow(
    `case c-no-violating (${file}:1) has no violatingStep (a step that breaks the line, written before any run): build its case again`
  );
  const userStep = { ...f1Case("c-user-violating"), violatingStep: f1Messages()[0] };
  const empty = {
    ...f1Case("c-empty-violating"),
    violatingStep: { role: "assistant", content: [{ type: "text", text: "  " }] },
  };
  const bad = caseFile("bad-violating", [userStep, empty]);
  expect(() => loadCases(bad, base)).toThrow(
    `case c-user-violating (${bad}:1) is not a case: violatingStep: is not an assistant message`
  );
  expect(() => loadCases(bad, base)).toThrow(
    `case c-empty-violating (${bad}:2) is not a case: violatingStep: holds no text and no tool call`
  );
});

test("refuses a skillReads index that is not a read result of the skill's files", () => {
  const base = alphaDir("base-reads");
  const user = { ...f1Case("c-user"), skillReads: [0, 2, 4] };
  const outside = { ...f1Case("c-outside"), skillReads: [2, 4, 40] };
  const gamma = { ...f1Case("c-gamma"), skillReads: [2, 4, 6] };
  const missing = { ...f1Case("c-missing"), skillReads: [2] };
  const file = caseFile("reads", [user, outside, gamma, missing]);
  expect(() => loadCases(file, base)).toThrow(
    [
      "refused cases:",
      `case c-user (${file}:1): skillReads index 0 is not a read result`,
      `case c-outside (${file}:2): skillReads index 40 is not a read result`,
      `case c-gamma (${file}:3): skillReads index 6 answers a read of skill://gamma, not a read of a skill://alpha file`,
      `case c-missing (${file}:4): the read result at index 4 (skill://alpha/references/x.md) is not in skillReads`,
    ].join("\n")
  );
});

test("refuses a file of more than one skill, and an id given twice", () => {
  const base = alphaDir("base-skills");
  const beta = { ...f1Case("c-beta"), skill: "beta", skillReads: [] };
  const file = caseFile("skills", [f1Case("c1"), f1Case("c1")]);
  expect(() => loadCases(file, base)).toThrow(`case c1 (${file}:2): its id is given twice`);
  // beta has no SKILL.md line 9 of alpha's text; only the skill names matter here.
  const mixed = caseFile("mixed", [f1Case("c1"), { ...beta, baseFile: "SKILL.md" }]);
  expect(() => loadCases(mixed, base)).toThrow(
    `${mixed} holds cases of more than one skill: alpha and beta (one skill per case file)`
  );
});

test("skillReadFile reads skill:// URLs of the skill only", () => {
  expect(skillReadFile("alpha", "skill://alpha")).toBe("SKILL.md");
  expect(skillReadFile("alpha", "skill://alpha/")).toBe("SKILL.md");
  expect(skillReadFile("alpha", "skill://alpha/references/x.md")).toBe("references/x.md");
  expect(skillReadFile("alpha", "skill://alpha/references/x%20y.md")).toBe("references/x y.md");
  expect(skillReadFile("alpha", "skill://alphabet")).toBeUndefined();
  expect(skillReadFile("alpha", "skill://beta")).toBeUndefined();
  expect(skillReadFile("alpha", "skill://alpha/../beta/SKILL.md")).toBeUndefined();
  expect(skillReadFile("alpha", "skill://alpha/SKILL.md:1-5")).toBeUndefined();
  expect(skillReadFile("alpha", "/profiles/p/agent/skills/alpha/SKILL.md")).toBeUndefined();
});
