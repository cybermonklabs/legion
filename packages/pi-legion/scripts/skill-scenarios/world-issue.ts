// The words of a phase worker's Dispatch issue, the greet() issue every worker run's world stands
// on (seed.ts tester-proof and seed-handoffs.ts's planSeed write them): its repository, its title,
// its spec, its Verification document and its plan. They are kept here, apart from the modes that
// write them, because rig.test.ts reads them beside the repository rig.sh's fixture builds: a
// sentence of the repository that is a text finding is one only because the spec says the code is
// right and the sentence wrong, and a check that covers its requirement is one only against the
// spec's words. It imports nothing, so a reader of the words loads no Dispatch client.

/** The repository the world is on: the project's default repository (seed.ts project), which the
 * issue resolves to, and rig.sh's `worker_repo`, which every world's remote, pull request and gh
 * answer names. rig.test.ts holds the two to one name. */
export const WORLD_REPO = "fernbrook/widgets";

/** The issue's title. */
export const WORLD_TITLE = "greet(): a greeting helper for the widgets CLI";

/** The issue's spec: what the CLI prints and where the name is trimmed (the Goal and R3), what it
 * does with no name or a blank one (R2), and when the repository's CI runs (Context). Those last
 * two are what make each text finding a world carries closable by text alone: docs/cli.md's
 * sentence that the name is printed as given contradicts R3, and the workflow's comment naming a
 * push trigger contradicts Context, while the code meets both, so correcting the sentence is the
 * one correct close and changing the code to fit it breaks the spec. */
export const WORLD_SPEC = [
  "## Goal",
  "",
  "`greet(name)` returns `Hello, <name>!` for the name it is given, and `bun greet.ts <name>` prints that greeting for the name with the whitespace around it trimmed: the command trims the name, and `greet()` greets what it is handed.",
  "",
  "## Requirements",
  "",
  ":::requirement{#r1}",
  "**R1.** `bun greet.ts Ada` prints `Hello, Ada!` and exits 0. From: the issue.",
  ":::",
  "",
  ":::requirement{#r2}",
  "**R2.** `bun greet.ts` with no name, or a blank one, exits 2 and prints `usage: greet.ts <name>` to stderr. From: the issue.",
  ":::",
  "",
  ":::requirement{#r3}",
  "**R3.** Surrounding whitespace in the name is trimmed: `bun greet.ts '  Ada  '` prints `Hello, Ada!`. From: the issue.",
  ":::",
  "",
  "## Context",
  "",
  "The repository's CI runs on pull requests alone. Every change reaches `main` through a pull request whose head CI has already tested, so no workflow of the repository runs on a push to `main`.",
].join("\n");

/** The requirement blocks of WORLD_SPEC, by the reference a pointer of another document of the
 * issue names each by (`dispatch://<issue>/spec#b-<id>`). */
const requirement = (issue: string, id: string) => `dispatch://${issue}/spec#b-${id}`;
const REQUIREMENTS = ["r1", "r2", "r3"];

/** What the issue's one check covers, each kind once. `r2` covers all of R2: no name and a blank
 * one. `no-name` covers the no-name case alone, its words and its negative control exactly as
 * every world's check had them before 2026-11-16: the world whose CLI dropped the blank-name guard
 * keeps that check, since a check that missed the blank name is how that defect reached review
 * (the architect's ruling of 2026-11-16), and widening it would leave a recorded pass that the
 * head fails. `doc-example` covers all of R2 as `r2` does, and compares the output of `bun
 * greet.ts Ada` with the example line DOCS_FILE gives for that command (rig.sh's
 * fixture_round_files writes it in every world): threads-closed's `crossing`, whose reviewer's
 * commit rewords that line, so the tester's earlier pass does not hold at that commit
 * (Verification v47's k3). */
export const CHECK_COVERS = ["r2", "no-name", "doc-example"] as const;
export type CheckCovers = (typeof CHECK_COVERS)[number];

/** Whether `word` is a kind of CHECK_COVERS. */
export function isCheckCovers(word: string | undefined): word is CheckCovers {
  return CHECK_COVERS.some((kind) => kind === word);
}

/** The issue's Verification document: the one `before_merge` check `k1` every seeded result and
 * every role's own run is recorded against, serving the three requirements, in the words
 * `covers` names. */
export function worldVerification(issue: string, covers: CheckCovers): string {
  const serves = REQUIREMENTS.map((id) => requirement(issue, id)).join(" ");
  const r2 =
    "`bun greet.ts <name>` prints the greeting and exits 0, trims the name, and exits 2 with the usage line `usage: greet.ts <name>` on stderr both when no name is given and when the name is blank.";
  const check =
    covers === "r2"
      ? [
          `:::check{#k1 serves="${serves}" surface="cli" run="bun greet.ts Ada" when="before_merge" negative_control="bun greet.ts with no name, and bun greet.ts with a name of spaces alone, each exit 2 and print the usage line to stderr."}`,
          r2,
        ]
      : covers === "doc-example"
        ? [
            `:::check{#k1 serves="${serves}" surface="cli" run="bun greet.ts Ada" when="before_merge" negative_control="bun greet.ts with no name, and bun greet.ts with a name of spaces alone, each exit 2 and print the usage line to stderr; an example line of ${DOCS_FILE} for bun greet.ts Ada that gives another greeting than the command prints fails the comparison."}`,
            `${r2} The output of \`bun greet.ts Ada\` is compared with the example line that \`${DOCS_FILE}\` gives for that command, and the check passes only where the two give the same greeting.`,
          ]
        : [
            `:::check{#k1 serves="${serves}" surface="cli" run="bun greet.ts Ada" when="before_merge" negative_control="bun greet.ts with no name exits 2 and prints the usage line to stderr."}`,
            "`bun greet.ts <name>` prints the greeting, trims the name, and exits 2 with a usage line when no name is given.",
          ];
  return ["# Verification", "", ...check, ":::"].join("\n");
}

/** The files of a change the plan holds a task for beyond the CLI and its tests: the two a round
 * after the implementer reads (rig.sh's fixture_round_files). */
export const DOCS_FILE = "docs/cli.md";
export const WORKFLOW_FILE = ".github/workflows/greet.yml";

/** The issue's plan.md, as its planner wrote it before the implementer began: a `plan_task` for
 * the CLI and one for R2, each serving the requirements it builds, and one for each of DOCS_FILE
 * and WORKFLOW_FILE that `files`, the files the implementer's change touches, holds. A plan that
 * named a file the change leaves alone would be a task the change left undone. */
export function worldPlan(issue: string, files: readonly string[]): string {
  const task = (id: string) => `dispatch://${issue}/artifact/plan-md#b-${id}`;
  const tasks = [
    `:::plan_task{#t1 serves="${requirement(issue, "r1")} ${requirement(issue, "r3")}"}`,
    "**T1.** `greet.ts`: `greet(name)` returns `Hello, <name>!`, and the command trims its argument once and prints `greet()` of it. Tests: `greet()` alone, and the command run with `Ada` and with `  Ada  `.",
    ":::",
    "",
    `:::plan_task{#t2 serves="${requirement(issue, "r2")}" needs="${task("t1")}"}`,
    "**T2.** The command prints `usage: greet.ts <name>` to stderr and exits 2 when the trimmed name is empty, which no name and a blank one both give. Tests: the command run with no name and with a name of spaces alone.",
    ":::",
  ];
  if (files.includes(DOCS_FILE))
    tasks.push(
      "",
      `:::plan_task{#t3 serves="${REQUIREMENTS.map((id) => requirement(issue, id)).join(" ")}" needs="${task("t2")}"}`,
      "**T3.** `docs/cli.md` documents `bun greet.ts <name>` as R1 to R3 state it.",
      ":::"
    );
  if (files.includes(WORKFLOW_FILE))
    tasks.push(
      "",
      `:::plan_task{#t4 needs="${task("t2")}"}`,
      "**T4.** `.github/workflows/greet.yml` runs the tests on every pull request, as the repository's CI runs (the spec's Context).",
      ":::"
    );
  return ["# Plan", "", ...tasks].join("\n");
}
