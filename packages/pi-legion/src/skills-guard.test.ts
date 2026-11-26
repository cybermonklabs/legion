import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync, statSync } from "node:fs";
import * as path from "node:path";
import {
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  LEGION_PLAN_REASON_MAX,
} from "@legion/contracts/legion-api";
import {
  brokenRelativeLinks,
  brokenSkillLinks,
  bundledAgents,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  references,
  shippedAgents,
  skillLinks,
  stageSkills,
  unresolvedReferences,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-legion, staged here as
// its prepack stages it. Everything a Legion agent reads that can link into a skill is a linking
// root: the staged skills themselves and the daemon's prompts (its role prompts in roles/ and its
// overlays in go/). A `skill://` link resolves by name through Oh My Pi's discovery, so it is held to
// the repository's skills/, where both plugins' skills live: `skill://dispatch/…` from this
// partition is valid because the Envoy plugin, installed beside this one, ships it.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const promptsRoot = path.join(REPO_ROOT, "packages/daemon/internal/prompts");
const agentsRoot = path.join(REPO_ROOT, "packages/pi-legion/agents");
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-legion");
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

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged, promptsRoot]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
});

// Every reference a Legion role can be pointed along resolves against the two packed plugins:
// those in the texts a role reads — the skills both plugins stage, the agents this plugin ships
// and the daemon's prompt parts — and those the daemon and the plugins' own code write, in a
// message, a refusal or a comment, read whole from every non-test Go file of the daemon and
// every non-test TypeScript file of the sources the plugins bundle. A `skill://` reference or a
// `skills/<name>/<path>` path names a skill one partition stages, a file of it, and a heading of
// that file for its `#anchor`; a `task(agent="<name>")` names an agent this plugin ships or Oh My
// Pi bundles. Each group of files must hold a reference, so a reader that found none cannot pass
// while checking nothing.
function everyReference(envoyStaged: string) {
  const typeScript = [
    "packages/pi-shared/src",
    "packages/pi-legion/src",
    "packages/envoy-client/src",
  ]
    .flatMap((root) => files(path.join(REPO_ROOT, root)))
    .filter(
      (file) => file.endsWith(".ts") && !file.endsWith(".test.ts") && !file.includes("/__tests__/")
    );
  const go = files(path.join(REPO_ROOT, "packages/daemon")).filter(
    (file) => file.endsWith(".go") && !file.endsWith("_test.go")
  );
  const fromRepository: [string, string[]][] = [
    ["the agents", files(agentsRoot).filter((file) => file.endsWith(".md"))],
    ["the prompt parts", files(promptsRoot).filter((file) => file.endsWith(".md"))],
    ["the daemon's Go", go],
    ["the plugins' TypeScript", typeScript],
  ];
  const found = [
    {
      group: "the staged skills",
      held: [staged, envoyStaged].flatMap((root) =>
        files(root).flatMap((file) =>
          references(path.join("skills", path.relative(root, file)), readFileSync(file, "utf8"))
        )
      ),
    },
    ...fromRepository.map(([group, sources]) => ({
      group,
      held: sources.flatMap((file) =>
        references(path.relative(REPO_ROOT, file), readFileSync(file, "utf8"))
      ),
    })),
  ];
  expect(found.filter(({ held }) => held.length === 0).map(({ group }) => group)).toEqual([]);
  return found.flatMap(({ held }) => held);
}

test("every reference a shipped text or the daemon names resolves against the packed plugins", () => {
  const envoyStaged = stageSkills("@sjawhar/pi-envoy");
  try {
    const skillReferences = everyReference(envoyStaged).filter(
      (reference) => !("agent" in reference)
    );
    expect(unresolvedReferences(skillReferences, [staged, envoyStaged], new Set())).toEqual([]);
  } finally {
    rmSync(path.dirname(envoyStaged), { recursive: true, force: true });
  }
});

// The agents Oh My Pi bundles are read from the Oh My Pi binary itself, as
// packages/pi-envoy/src/skills-guard.test.ts's agent test reads them: LEGION_TEST_OMP names it, and
// a run without it skips this test. CI runs this file twice: pi-legion-build's step "Check the
// skills against their guard", the pull request's half, installs no Oh My Pi, so the test skips
// there; pi-legion's "Test pi-legion", the merge queue's half, runs it on the pin
// .github/actions/install-omp installs and exports as LEGION_TEST_OMP.
const omp = process.env.LEGION_TEST_OMP;
test.skipIf(omp === undefined)(
  "every task agent a shipped text or the daemon names is one this plugin ships or Oh My Pi bundles",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset");
    const envoyStaged = stageSkills("@sjawhar/pi-envoy");
    try {
      const agentReferences = everyReference(envoyStaged).filter(
        (reference) => "agent" in reference
      );
      const available = new Set([
        ...shippedAgents(path.dirname(agentsRoot)),
        ...(await bundledAgents(omp)),
      ]);
      expect(unresolvedReferences(agentReferences, [], available)).toEqual([]);
    } finally {
      rmSync(path.dirname(envoyStaged), { recursive: true, force: true });
    }
  },
  60_000
);

// Reads by filesystem path stop at 300 lines, so a worker reaches a reference only through a
// `skill://legion-worker/<path>` link from somewhere it reads.
test("every legion-worker reference is linked from a skill or a prompt", () => {
  const workerRoot = path.join(staged, "legion-worker");
  const linked = new Set(
    skillLinks([staged, promptsRoot])
      .filter(({ name }) => name === "legion-worker")
      .map(({ target }) => target)
  );
  const unlinked = files(path.join(workerRoot, "references"))
    .map((file) => path.relative(workerRoot, file))
    .filter((reference) => !linked.has(reference));
  expect(unlinked).toEqual([]);
});

// Where a tree owns a clear defect, the controller's reply under the finder's comment or ask names
// the owner: the finder writes to it, or, for a person's `Hand it to the owning tree` on an old
// ask, the reply mentions the owner, which reads the answer and the evidence in that thread, and
// the finder writes nothing. A finder that reads a reply it has no rule for opens a second ask, so
// every `Not filed:` reply of the controller's that names a role is one the finder's findings.md,
// or its steps file, acts on, word for word but its placeholders.
test("every Not filed reply the controller writes that names an owner is one the finder acts on", () => {
  // Both sides are read by one map, so they are parsed alike. Each side is its reference and the
  // steps file its detail moved to under the skill budget.
  const [controller = new Set<string>(), finder = new Set<string>()] = [
    [
      "legion-controller/references/finding-asks.md",
      "legion-controller/references/finding-asks-steps.md",
    ],
    ["legion-worker/references/findings.md", "legion-worker/references/findings-steps.md"],
  ].map(
    (files) =>
      new Set(
        files.flatMap((file) =>
          [
            ...readFileSync(path.join(staged, file), "utf8").matchAll(
              /`(Not filed: [^`]*role:[^`]*)`/g
            ),
          ].map((match) => (match[1] ?? "").replace(/<[^>]*>/g, "<>"))
        )
      )
  );
  expect(controller.size).toBeGreaterThan(0);
  expect([...controller].filter((form) => !finder.has(form))).toEqual([]);
});

// Whitespace is collapsed before a match, as shipped-agents.test.ts collapses it, so a pin survives
// a rewrap of the file it reads.
const collapsed = (file: string) => readFileSync(file, "utf8").replace(/\s+/g, " ");

// The completion that ends the retro rates the plan, and this skill's step that gives the two
// fields is the one place the rule of the rating is stated in prose; the daemon refuses a
// completion that breaks the rule (the daemon's planRatingOf and planRatingRefusal). So the step
// names the two fields, the three codes the daemon refuses with, and the scale and the bound of
// the reason as the contract holds them: a new scale or bound fails here until the skill states
// it. The bound is pinned with the space before it, since step 5's "32,000 characters" holds
// "2,000 characters" without it. This shows the text ships, never that a role follows it.
test("the retro skill names the rating's fields, its refusals, its scale and its bound", () => {
  const retro = collapsed(path.join(staged, "legion-retro", "SKILL.md"));
  for (const token of [
    "`plan_rating`",
    "`plan_reason`",
    "`PLAN_RATING_REQUIRED`",
    "`PLAN_RATING_UNEXPECTED`",
    "`PLAN_RATING_INPUT`",
    `${LEGION_PLAN_RATING_LEAST} (the plan cost the work)`,
    `${LEGION_PLAN_RATING_MOST} (the plan fit the work)`,
    ` ${LEGION_PLAN_REASON_MAX.toLocaleString("en-US")} characters`,
  ]) {
    expect(retro).toContain(token);
  }
});

// Only a review that requests changes counts a review round (the daemon's recordRound): a tester's
// fail and a move back, the architect's from `accepting` among them, count none. The two guides
// that say what the cap counts say so too, and each sentence that counted every move back is gone.
// A move made at a conflicting head raises no pair, whoever made it (recordPass), so the concepts
// page says that of any such move.
// The guides are read where they live in the repository.
test("the guides say only a review that requests changes counts a round", () => {
  const guide = (relative: string, duties: RegExp[]) => {
    const text = collapsed(path.join(REPO_ROOT, "docs/site/src/content/docs/legion", relative));
    return duties.filter((duty) => !duty.test(text)).map((duty) => `${relative} lacks ${duty}`);
  };
  expect([
    ...guide("concepts.md", [
      /Only a review that requests changes counts a round: a tester's fail and a move back by a worker or the architect count none\./,
      /A request for changes made while the record holds the head conflicting with its base is a conflict round/,
      /a move made while the head conflicts with its base raises no pair/,
      /^(?![\s\S]*Every move back to an earlier phase counts a round)/,
    ]),
    ...guide("troubleshooting.md", [
      /only a review that requests changes raises the count/,
      /^(?![\s\S]*every move back to an earlier phase raises the count)/,
    ]),
  ]).toEqual([]);
});

// skills/AGENTS.md bounds a skill's SKILL.md and each of its references at 150 lines and 8 KB,
// counted here as 8,000 bytes. Every file of the Legion role skills is held to it, so a file that
// grows past it moves its detail one level down before it ships.
test("every file of the Legion role skills fits the skill budget", () => {
  const budgeted = files(staged).filter((file) =>
    /^legion-[a-z-]+\/(SKILL|references\/[^/]+)\.md$/.test(path.relative(staged, file))
  );
  const over = budgeted.flatMap((file) => {
    const text = readFileSync(file, "utf8");
    const bytes = Buffer.byteLength(text);
    const lines = text.split("\n").length - (text.endsWith("\n") ? 1 : 0);
    return bytes > 8_000 || lines > 150
      ? [`${path.relative(staged, file)}: ${lines} lines, ${bytes} bytes`]
      : [];
  });
  expect(over).toEqual([]);
});

// The entry skill each Legion role is served, its `SKILL.md` as this plugin stages it, grows by
// no more than the 500 bytes the spec approved over its size at the base. Each ceiling is that
// file's size measured at ecd967b17a4e, plus 500.
const ENTRY_SKILL_CEILINGS = [
  { skill: "legion-worker", ceiling: 7_968 + 500 },
  { skill: "legion-architect", ceiling: 7_829 + 500 },
  { skill: "legion-controller", ceiling: 7_767 + 500 },
];
test("each served entry skill stays within its base plus 500 bytes", () => {
  const over = ENTRY_SKILL_CEILINGS.flatMap(({ skill, ceiling }) => {
    const bytes = statSync(path.join(staged, skill, "SKILL.md")).size;
    return bytes > ceiling
      ? [`${skill}/SKILL.md: ${bytes} bytes, over its ceiling of ${ceiling}`]
      : [];
  });
  expect(over).toEqual([]);
});
