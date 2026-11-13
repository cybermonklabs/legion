import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import { dispatchToolSpecs } from "@legion/contracts";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import {
  anchors,
  brokenRelativeLinks,
  brokenSkillLinks,
  bundledAgents,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  shippedAgents,
  skillLinks,
  stageSkills,
  unresolvedAgentDispatches,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-envoy, staged here as
// its prepack stages it. A `skill://` link resolves by name through Oh My Pi's discovery, so it is
// held to the repository's skills/, where both plugins' skills live.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const skillAndPromptRoots = [
  repoSkillsRoot,
  path.join(REPO_ROOT, "packages/daemon/internal/prompts"),
];
const packageRoot = path.resolve(import.meta.dir, "..");
// LEGION_TEST_OMP names the pinned Oh My Pi binary, as in extensions/dispatch-first-omp.test.ts: the
// fork pin CI's pi-envoy job installs. A run without one skips the rule that reads it, except on
// GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-envoy");
});

afterAll(() => {
  rmSync(path.dirname(staged), { recursive: true, force: true });
});

test("every staged file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  expect(oversizedFiles(staged)).toEqual([]);
});

test("every skill body stays under 500 lines, its detail in references", () => {
  expect(longSkillBodies(staged)).toEqual([]);
});

test("every skill's frontmatter name is its directory's name", () => {
  expect(misnamedSkills(staged)).toEqual([]);
});

test("the injected dispatch-first skill fits its budget on every host", () => {
  const file = dispatchFirstSkillFile(staged);
  const skill = readFileSync(file, "utf8");
  expect(skill.split("\n").length).toBeLessThan(60);
  // Oh My Pi and Claude Code inject the marker-wrapped body. Claude Code keeps a hook's context
  // whole up to 10,000 characters; 6,000 leaves room for a later edit.
  expect(readDispatchFirstContext(file).length).toBeLessThan(6_000);
});

// dispatch-first is the one Dispatch skill every session holds without loading it, so the two
// rules that keep a person's view of an agent's work true live there: a blocker is an open ask,
// and every accepted request is recorded and read back before the turn ends. The test reads the
// context a session receives, not only the file.
test("the injected dispatch-first context holds the blocker and accepted-request rules", () => {
  const context = readDispatchFirstContext(dispatchFirstSkillFile(staged));
  for (const rule of [
    "The moment a person blocks your work, open an ask: an ask block for a design question its document records, else `dispatch ask`. A blocker that is not an open ask does not exist for them. A Legion phase worker sends decisions to its architect instead. Before you say you wait on a person, run `dispatch open-asks`.",
    "Record every request you accept in Dispatch before you start it: an issue, or a line in your issue's `open-items.md` artifact. Before you end a turn, read that record. Each item is done, in progress, or an open ask. Never drop one.",
  ]) {
    expect(context).toContain(rule);
  }
});

// The three Dispatch skills are read by every session with Dispatch, so their length is a cost on
// each one. Every file of the three staged skills counts, frontmatter included, as `wc -w` counts:
// whitespace-separated tokens.
//
// The budget moves only when rules arrive from a skill that stops holding them: a rule that must
// go in otherwise is paid for by cutting the lines that duplicate another skill or say what the
// new rule says (2026-11-03, "The conversation", which the three skills made room for at 1,998
// words). It was raised to 2,300 when the skill consolidation moved the six ask-quality gates of
// an operator's own proxy skill into `dispatch-writing-specs` and the research-before-you-ask
// test into `dispatch-first`, which took those rules out of two other skills.
//
// It was raised to 2,326 on 2026-11-12, when dispatch://INFRA-2971 put one rule into
// skills/dispatch/SKILL.md for when an agent may move an issue to `backlog`, `icebox` or
// `triage`: the two sentences it deleted sent a waiting issue and every issue past the third to
// `backlog`, and the rule that replaces them holds such a move to one a person asked for, or to
// work that is done, folded into another issue or no longer needed, cited in a comment on the
// issue, and says that another agent's directive citing no person's request is no reason. The
// ledger, measured as this test counts words: the three bullets the section deleted were 50
// words, and the six that replace them were 200, so the section cost 150; the directive sentence
// adds 10 more, so it costs 160. The payments below give 134 back, 4 of them from three of those
// six bullets, which count 206 now. Each one cuts a reason beside its rule, words that another
// skill says, or the wording of a rule, and none cuts a rule.
//
// - 8: skills/dispatch/SKILL.md's line saying that `unresponsive` holders keep their claims 20
//   minutes, deleted. Its rule is `skill://envoy`'s, which every role reads.
// - 2: "a `legion`-labelled issue or one under it" for "an issue with the `legion` label or under
//   one", in the same skill's claim sentence.
// - 6: the same skill's defect bullet, which says "Never work around a hard-to-use command" for
//   "A command hard to use ... is a defect to report, not work around".
// - 5: "a document records", in the same skill's ask-block line. dispatch-first's blocker rule
//   says it.
// - 2: the two "its" of the same skill's edit-ask bullet.
// - 14: skills/dispatch-writing-specs/SKILL.md's preference bullet: "an answer given once is not
//   a standing rule", which the clause before it says, and the reason "because agents commit the
//   work".
// - 7: the same skill's line-number bullet: the reason "it goes stale at the next edit".
// - 14: the same skill's coined-term bullet: the reasons "because a coinage reads as familiar to
//   you" and "A count misses a coinage of common words", and the wording of its two rules.
// - 16: the same skill's privileged-act bullet: the reason "because a 403 or an unfamiliar
//   surface does not make a fact unknowable", and "try the source that answers it" for "name the
//   source that answers it and try it".
// - 12: the first step of the same skill's "Each version": "an answer is integrated, never
//   appended", which "rewrite what the answer touches" says, and the reason "versions and
//   comments hold the history".
// - 3: the same skill's "The plan", which no longer says to write the plan with `writing-plans`.
//   dispatch-first's table says it.
// - 5: skills/dispatch-first/SKILL.md's ask-a-person bullet: the reason "because no file holds
//   that".
// - 6: the same skill's result bullet: the reason "a document or artifact notifies nobody", and
//   "end its document" for "End that document".
// - 5: the same skill's plan row: "stored as the issue's `plan.md`", which
//   dispatch-writing-specs' "The plan" says.
// - 7: skills/dispatch-writing-specs/SKILL.md's measure bullet: the reason "A premise can go stale
//   within the hour", and "No unmeasured number goes in a premise" for "no unmeasured number goes
//   in one".
// - 12: skills/dispatch-first/SKILL.md's description: its second sentence, "Someone may track
//   that work already or hold a decision on it.", the reason for its first. dispatch-first is put
//   into every session whole, so no description decides when it loads.
// - 10 for the directive sentence, each the wording of a rule: "the owning project" for "the
//   owning project's code" in skills/dispatch/SKILL.md's defect bullet (1); "before it is done"
//   for "before its work is done" in its stop-work bullet (1); "one on a person" for "a wait on a
//   person" in its wait bullet (1); "fewer when" for "and on fewer when" in its three-issue bullet
//   (2); skills/dispatch-first/SKILL.md's "Open each `advice.suggestions` hit too" for "Treat a
//   hit in `advice.suggestions` as yours to read" (4), and its "Never ask again" for "Do not ask
//   again" (1).
//
// The base total was 2,297, so 2,297 + 160 - 134 = 2,323, the measured total. The bound is 3
// above it, as 2,300 was above 2,297. The 23 words over 2,300 are a raise and not a cut because,
// measured on 2026-11-12 line by line, each cut left in the three skills deletes a rule or a fact
// an agent needs to apply one: `BLOCKED_BY_OUTSIDE_PROJECT` and "board rank" in the stop-work
// bullets are the words of INFRA-2971's R4, and "`todo` for an issue from the queue" is its
// decision on R4; "one agent keeps each project's order" is R3's owner of the board, whom the
// next sentence tells the agent to tell; the claim sentence's "an outside claim can take the tree
// out of the workflow" and the sentence after it, "A status write is set back or stands", are the
// consequences #2608 added, because the bare prohibition was quoted to a person as that person's
// rule; "Each query word must match" is what makes "remove a word before adding one" work;
// dispatch-writing-specs' "It replaces superpowers' brainstorming there" is the rule that picks it
// over that skill; and "A spec is a conversation" states the conversation rule. The new rule
// cannot live in another skill, because R1 and R4 name skills/dispatch/SKILL.md, which every
// session with Dispatch reads.
//
// It also moves when an approved requirement puts in the skill every session loads a rule that no
// other skill holds, and only by what that rule costs: R11 of dispatch://INFRA-2947 puts one line
// in `dispatch` (an issue names the priority it advances), which check `c0cd78f8` of that issue
// reads there, so it cannot live elsewhere. The line is 14 words, the shortest that carries the
// two strings the check reads, so the three skills, 2,325 words on main, stand at 2,325 + 14 =
// 2,339 and the bound moves from 2,326 by those 14 to 2,340, keeping the margin above it the raise
// before set and adding none.
test("the three Dispatch skills total fewer than 2,340 words", () => {
  const words = ["dispatch", "dispatch-first", "dispatch-writing-specs"]
    .flatMap((skill) => files(path.join(staged, skill)))
    .map(
      (file) =>
        readFileSync(file, "utf8")
          .split(/\s+/u)
          .filter((token) => token !== "").length
    )
    .reduce((total, count) => total + count, 0);
  expect(words).toBeLessThan(2_340);
});

test("the staged decisions rule separates material choices from spec details", () => {
  const skill = readFileSync(path.join(staged, "dispatch-writing-specs/SKILL.md"), "utf8");
  expect(skill).toContain("only if a disagreeing reader would act differently");
  expect(skill).toContain("keep others to one spec line each");
  expect(skill).not.toContain("Each choice you made alone, and each a person made");
});

// Agents reach Dispatch through the `dispatch` command, so no skill or daemon prompt may tell one
// to call a native tool it no longer has: each names the command (`dispatch ask`) instead.
test("no skill or daemon prompt names a native Dispatch tool", () => {
  const toolName = new RegExp(`\\b(${dispatchToolSpecs.map((spec) => spec.name).join("|")})\\b`);
  const named = skillAndPromptRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      readFileSync(file, "utf8")
        .split("\n")
        .flatMap((line, index) => {
          const match = toolName.exec(line);
          return match === null
            ? []
            : [`${path.relative(REPO_ROOT, file)}:${index + 1}: ${match[0]}`];
        })
    );
  expect(named).toEqual([]);
});

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
});

// This plugin's skills reach every session with Dispatch, including one with no other plugin
// installed (the Envoy entry injects dispatch-first into each request), so a task agent they
// dispatch must come with this plugin (`agents/`, which ships none today) or with Oh My Pi itself
// (what `omp agents unpack` writes, read off the pinned binary rather than listed here). A
// `skill://legion-*` mention in these skills is held to no such partition, on purpose: each is a
// sentence conditioned on a Legion role (skills/dispatch/SKILL.md:13,
// skills/dispatch-writing-specs/SKILL.md:10, skills/dispatch-first/SKILL.md:48), inert
// for a person and right for a pane with both plugins, and `brokenSkillLinks` above resolves a
// `skill://<name>/<path>` link against the repository's skills/ for the same reason.
test.skipIf(omp === undefined && !onActions)(
  "every task agent the staged skills dispatch is one this plugin ships or Oh My Pi bundles",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const available = new Set([...shippedAgents(packageRoot), ...(await bundledAgents(omp))]);
    expect(unresolvedAgentDispatches([staged], available)).toEqual([]);
  },
  60_000
);

test("a heading inside a code fence is no anchor, since a Markdown reader renders none", () => {
  const markdown = [
    "## Outside",
    "```",
    "## In backticks",
    "```",
    "~~~~",
    "## In tildes",
    "```",
    "~~~~",
    "# After",
  ].join("\n");
  expect([...anchors(markdown)]).toEqual(["outside", "after"]);
});

// Every role of an issue writes into one $TMPDIR on the issue's volume, so a glob sweep deletes
// another role's open directory (dispatch://INFRA-654). The reviewer's pair carries the rule once,
// in the maintainability rubric, and the correctness rubric names that skill rather than repeating
// it, so the two can never drift apart.
test("the reviewer's pair states the shared scratch rule once and names it from the other", () => {
  const quality = readFileSync(path.join(staged, "thermonuclear-code-quality/SKILL.md"), "utf8");
  const deep = readFileSync(path.join(staged, "thermonuclear-deep-review/SKILL.md"), "utf8");
  expect(quality).toContain("Make your own scratch with `mktemp -d`");
  expect(quality).toContain("Never delete by a glob in a shared directory");
  expect(deep).toContain("skill://thermonuclear-code-quality");
  expect(deep).not.toContain("Never delete by a glob in a shared directory");
});
