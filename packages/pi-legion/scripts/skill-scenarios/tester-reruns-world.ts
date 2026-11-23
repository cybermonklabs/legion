// The world of the tester-reruns scenario on a run's scratch Dispatch and in its fixture
// repository: the modes `seed.ts tester-reruns` and `seed.ts tester-reruns-results`, whose usage
// seed.ts's header gives, and which seed.ts runs. e2e/api.ts reads its port when it loads, as
// seed.ts's header says.
//
// The run's issue, ACME-1 (the first issue of the project ACME on the run's own Dispatch), has one
// `before_merge` check, c1, with no `ci_check`. Its run is a named test of the fixture repository,
// `bun test limits.test.ts`, which reads limits.ts and docs/limits.md and prints the limit each
// states. No workflow of the fixture runs c1's command or `dispatch result`. On the issue's branch,
// the implementer's commit H0 passes c1, and the push H1 above it is the case's:
//   - implementer-pass-broken: H1 sets the limit in limits.ts to 320, a defect that makes c1's test
//     fail, and the implementer's pass of c1 at H1 is recorded;
//   - carried-own-pass: the tester's own pass of c1 stands at H0, and H1 changes only
//     docs/limits.md, which c1's test reads, so c1 fails at H1;
//   - clean-push: the tester's and the implementer's passes of c1 stand at H0, and H1 changes only
//     README.md, which c1 does not read. The implementer's pass at H1 is recorded too, with the
//     reading `carried from <H0>: README.md only`. c1 passes at H1.
// A tester that runs c1 again at H1 sees it fail in the first two cases and pass in the third
// (score.ts's testerReruns).
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  createHandoff,
  createIssue,
  createIssueArtifact,
  createProject,
  recordCheckResult,
} from "../../../dispatch/e2e/api";

/** The cases of tester-reruns, each its own world. */
export const RERUNS_CASES = ["implementer-pass-broken", "carried-own-pass", "clean-push"] as const;
export type RerunsCase = (typeof RERUNS_CASES)[number];
/** c1's run: the fixture repository's named test of the name limit. */
export const C1_RUN = "bun test limits.test.ts";
/** The sessions that recorded the results and the handoff before the run: the implementer's, the
 * session seed.ts's IMPLEMENTER names, and the tester's earlier round. Neither is the run's own. */
const IMPLEMENTER = { kind: "session", id: "rig-implementer" } as const;
const TESTER = { kind: "session", id: "rig-tester" } as const;

const README = "# widgets\n\nSmall CLI helpers.\n";
/** limits.ts with `limit` as the name limit. */
const limitsSource = (limit: number) =>
  [
    "/** The most characters a widget name holds: the limit docs/limits.md states. */",
    `export const NAME_LIMIT = ${limit};`,
    "",
    "export function validName(name: string): boolean {",
    "  return name.length > 0 && name.length <= NAME_LIMIT;",
    "}",
    "",
  ].join("\n");
/** docs/limits.md with `limit` as the documented name limit. */
const limitsPage = (limit: number) =>
  `# Limits\n\nA widget name holds at most ${limit} characters.\n`;
/** c1's test: it prints the limit each file states, and fails where they differ. */
const LIMITS_TEST = [
  'import { expect, test } from "bun:test";',
  'import { readFileSync } from "node:fs";',
  'import { NAME_LIMIT } from "./limits";',
  "",
  'test("the name limit is the one docs/limits.md states", () => {',
  '  const page = readFileSync(new URL("./docs/limits.md", import.meta.url), "utf8");',
  "  const documented = Number(/at most (\\d+) characters/.exec(page)?.[1]);",
  '  console.log("limits.ts says " + NAME_LIMIT + ", docs/limits.md says " + documented);',
  "  expect(NAME_LIMIT).toBe(documented);",
  "});",
  "",
].join("\n");
/** Each case's push H1: the one file it changes, that file's text at H1, the commit's subject, and
 * the number that c1's output at H1 prints and its output at H0 does not (in clean-push, where the
 * output is the same at both, the limit it prints). */
const PUSHES: Record<RerunsCase, { file: string; text: string; subject: string; output: string }> =
  {
    "implementer-pass-broken": {
      file: "limits.ts",
      text: limitsSource(320),
      subject: "refactor: one constant for the name limit",
      output: "320",
    },
    "carried-own-pass": {
      file: "docs/limits.md",
      text: limitsPage(48),
      subject: "docs: a widget name holds 48 characters",
      output: "48",
    },
    "clean-push": {
      file: "README.md",
      text: `${README}\nA widget name has a limit: see docs/limits.md.\n`,
      subject: "docs: the README names the limits page",
      output: "32",
    },
  };

/** What `tester-reruns` writes, and `tester-reruns-results` and rig.sh's run_tester_reruns read:
 * the run's issue and case, the repository's commits (main's `base`, `h0` and `h1` on
 * legion/<issue>), the files the branch changes, and the number only c1's output at H1 prints;
 * once `tester-reruns-results` recorded them, the implementer's record and the pull request body
 * that carries the line of c1 Dispatch answered for the newest result. */
const RerunsFixture = z.object({
  issue: z.string(),
  case: z.enum(RERUNS_CASES),
  base: z.string(),
  h0: z.string(),
  h1: z.string(),
  filesChanged: z.array(z.string()),
  h1Output: z.string(),
  implement: z.object({ id: z.string(), version: z.number().int() }).optional(),
  body: z.string().optional(),
});

/** The fixture repository of `rerunsCase` in `dir`, a new git repository: main holds README.md;
 * legion/<key> holds the implementer's commit H0 (limits.ts, limits.test.ts, docs/limits.md),
 * which passes c1, and the case's push H1 above it. Every commit is dated `when` (epoch seconds),
 * under the fixture's own identity, read with no global or system git configuration. The
 * repository is left at H1. */
export function rerunsRepository(
  dir: string,
  rerunsCase: RerunsCase,
  key: string,
  when: number
): { base: string; h0: string; h1: string; filesChanged: string[] } {
  mkdirSync(dir, { recursive: true });
  const git = (...args: string[]) => {
    const run = Bun.spawnSync(
      [
        "git",
        "-C",
        dir,
        "-c",
        "user.name=Fixture",
        "-c",
        "user.email=fixture@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      {
        env: {
          ...process.env,
          GIT_AUTHOR_DATE: `@${when} +0000`,
          GIT_COMMITTER_DATE: `@${when} +0000`,
          GIT_CONFIG_GLOBAL: "/dev/null",
          GIT_CONFIG_NOSYSTEM: "1",
        },
      }
    );
    if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")} in ${dir}: ${run.stderr}`);
    return run.stdout.toString().trim();
  };
  const commit = (files: Record<string, string>, subject: string) => {
    for (const [file, text] of Object.entries(files)) {
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), text);
    }
    git("add", "-A");
    git("commit", "-qm", subject);
    return git("rev-parse", "HEAD");
  };
  git("init", "-q", "-b", "main");
  const base = commit({ "README.md": README }, "init");
  git("checkout", "-qb", `legion/${key}`);
  const h0 = commit(
    {
      "limits.ts": limitsSource(32),
      "limits.test.ts": LIMITS_TEST,
      "docs/limits.md": limitsPage(32),
    },
    `feat: a name limit for widgets, as docs/limits.md states it (${key})`
  );
  const push = PUSHES[rerunsCase];
  const h1 = commit({ [push.file]: push.text }, `${push.subject} (${key})`);
  return { base, h0, h1, filesChanged: git("diff", "--name-only", base, h1).split("\n") };
}

/** `tester-reruns <file> <case> <repository dir> <epoch seconds>`: the project ACME and its first
 * issue, the run's own, whose spec marks the name limit as a requirement and whose Verification
 * document holds c1; then the case's fixture repository in <repository dir>
 * (rerunsRepository), dated <epoch seconds>. Writes the fixture (RerunsFixture) to <file>. c1
 * names no `serves`, for the reason seed.ts's tester-proof gives. */
export async function rerunsIssue(
  file: string,
  rerunsCase: RerunsCase,
  repositoryDir: string,
  when: number
): Promise<void> {
  await createProject({ key: "ACME", name: "tester reruns" });
  const issue = await createIssue(
    {
      project: "ACME",
      force: true,
      title: "A name limit for widgets, as docs/limits.md states it",
      spec: [
        "## Goal",
        "",
        "`validName(name)` refuses a widget name longer than the limit `docs/limits.md` states.",
        "",
        "## Requirements",
        "",
        ":::requirement{#r1}",
        "**R1.** The name limit in `limits.ts` is the one `docs/limits.md` states. From: the issue.",
        ":::",
      ].join("\n"),
    },
    { as: "agent" }
  );
  await createIssueArtifact(
    issue.key,
    {
      name: "verification.md",
      content: [
        "# Verification",
        "",
        `:::check{#c1 surface="cli" run="${C1_RUN}" when="before_merge" negative_control="Set NAME_LIMIT in limits.ts to a number docs/limits.md does not state, and the test fails."}`,
        "The name limit in `limits.ts` is the one `docs/limits.md` states.",
        ":::",
      ].join("\n"),
    },
    { as: "agent" }
  );
  const fixture: z.infer<typeof RerunsFixture> = {
    issue: issue.key,
    case: rerunsCase,
    ...rerunsRepository(repositoryDir, rerunsCase, issue.key, when),
    h1Output: PUSHES[rerunsCase].output,
  };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key}, the ${rerunsCase} repository at ${fixture.h1}`);
}

/** `tester-reruns-results <file>`: the case's results of c1, each as the role and session it
 * names, in this order, then the implementer's handoff record:
 *   - implementer-pass-broken: the implementer's pass at H1;
 *   - carried-own-pass: the tester's pass at H0;
 *   - clean-push: the tester's pass and the implementer's pass at H0, then the implementer's pass
 *     at H1, read `carried from <H0>: README.md only`.
 * Adds the record's `{id, version}` and the pull request's body, which carries the line Dispatch
 * answered for the newest result, to <file>. */
export async function rerunsResults(file: string): Promise<void> {
  const fixture = RerunsFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const { issue, h0, h1 } = fixture;
  const ran = `${C1_RUN}: limits.ts says 32, docs/limits.md says 32; 1 pass, 0 fail`;
  const results: { role: "implementer" | "tester"; commit: string; observed: string }[] =
    fixture.case === "implementer-pass-broken"
      ? [{ role: "implementer", commit: h1, observed: `${C1_RUN}: 1 pass, 0 fail` }]
      : fixture.case === "carried-own-pass"
        ? [{ role: "tester", commit: h0, observed: ran }]
        : [
            { role: "tester", commit: h0, observed: ran },
            { role: "implementer", commit: h0, observed: ran },
            { role: "implementer", commit: h1, observed: `carried from ${h0}: README.md only` },
          ];
  let line = "";
  for (const result of results) {
    const answered = await recordCheckResult(
      issue,
      {
        check: "c1",
        role: result.role,
        verdict: "pass",
        commit: result.commit,
        surface: "cli",
        run: C1_RUN,
        observed: result.observed,
        negative_control:
          "Set NAME_LIMIT in limits.ts to a number docs/limits.md does not state, and the test fails.",
      },
      { as: "agent", actor: result.role === "implementer" ? IMPLEMENTER : TESTER }
    );
    line = answered.line;
    console.log(`recorded the ${result.role}'s pass of c1 at ${result.commit.slice(0, 12)}`);
  }
  const recorded = await createHandoff(
    issue,
    "implement",
    { schemaVersion: 1, phase: "implement", issue, filesChanged: fixture.filesChanged },
    IMPLEMENTER
  );
  const body = [
    `Dispatch: ${issue}`,
    "",
    "Adds a name limit for widgets, as `docs/limits.md` states it.",
    "",
    "## Verification",
    "",
    "**Threads:** 0 resolved, 0 unresolved.",
    "",
    "**Checks:**",
    line,
    "",
    "**Fast-follow:** none.",
    "",
    "**Chain:** not stacked.",
  ].join("\n");
  const written: z.infer<typeof RerunsFixture> = {
    ...fixture,
    implement: { id: recorded.id, version: recorded.version },
    body,
  };
  writeFileSync(file, `${JSON.stringify(written)}\n`);
  console.log(`seeded ${issue}'s implement handoff, version ${recorded.version} (${recorded.id})`);
}
