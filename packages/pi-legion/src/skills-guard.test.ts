import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  LEGION_PLAN_REASON_MAX,
} from "@legion/contracts/legion-api";
import {
  brokenRelativeLinks,
  brokenSkillLinks,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  skillLinks,
  stageSkills,
} from "@legion/pi-shared/test/skills-guard";

// The Claude Code plugin is generated from this repository's skills/ and this package's agents/,
// and nothing commits packages/claude-envoy/legion/, so a check of what a Claude Code role loads
// builds a fresh one, as
// packages/claude-envoy/tests/legion-plugin-fresh.test.ts does. The build runs as its own command,
// the way stageSkills runs the prepack script: imported, that package's sources would be
// typechecked under this package's `tsc`, whose `lib` is older than the one they are written
// against.
const PLUGIN_BUILDER = path.join(REPO_ROOT, "packages/claude-envoy/scripts/build.ts");

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

// The merger merges the pull request itself, at the head it read, and the daemon submits no merge
// and changes no merge setting (dispatch://INFRA-714, R2): no skill a Legion pane loads, and no
// daemon prompt, may hand the merge back to the daemon. The pattern matches the claim made of the
// daemon and not its denial, so the verb follows `daemon` directly and names what is merged.
test("no skill or prompt says the daemon merges", () => {
  const saying = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") &&
        /daemon\s+(submits|arms|enqueues|enables|merges|turns)\s+(the|a|an|that|its|it|ready|auto)\b/i.test(
          readFileSync(file, "utf8")
        )
    )
    .map((file) => path.relative(REPO_ROOT, file));
  expect(saying).toEqual([]);
});

// An issue's disk dies with its pod (INFRA-80), so nothing writes the recovery marker
// `.legion/<issue>/workspace-recovered.json` any more: `workspace-init` lost it with the lost-disk
// verdict. The `Your workspace was recreated…` notice now reaches every resume in a new pod, so a
// skill or prompt that still sends an agent to the marker sends every resumed agent after a file
// that never exists.
test("no skill or prompt sends an agent to the workspace recovery marker, which nothing writes", () => {
  const sending = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") && readFileSync(file, "utf8").includes("workspace-recovered.json")
    )
    .map((file) =>
      file.startsWith(staged)
        ? path.join("skills", path.relative(staged, file))
        : path.relative(REPO_ROOT, file)
    );
  expect(sending).toEqual([]);
});

// The stale-plugin lane replaces a stale process at a paced wake and asks its worker nothing, so
// no daemon of this release opens a plugin update round: `supervise.PodRound` builds a pod
// round's id and nothing builds a plugin round's. The controller reads an `update-stalled` wake
// through its own reference, so a reference that still tells it a round may be
// `plugin:<daemon contract>` teaches it to read a round the daemon cannot open — which is what
// that reference did until this change, with nothing to see it. The rule is read off the
// daemon's own source rather than off the sentence, and it reads both ways: while nothing builds
// a plugin round id, no skill or prompt may promise one; the day something builds one again, the
// controller's own reference — the file that tells it how to read a stalled round — must name
// that round again, and this row fails until it does. The Go census beside it
// (prompts/roles_test.go's TestNoPartPromisesAnUpdateRoundNoDaemonCanOpen) asks the same of the
// embedded prompts; a row quoting the deleted prose would have passed the day the lane went.
test("no skill or prompt promises an update round no daemon can open", () => {
  const supervise = files(path.join(REPO_ROOT, "packages/daemon/internal/supervise"))
    .filter((file) => file.endsWith(".go") && !file.endsWith("_test.go"))
    .map((file) => readFileSync(file, "utf8"));
  // A reader that found no opener at all would pass below while checking nothing, so the pod
  // round's own constructor is the control: it is the kind a skill may describe.
  expect(supervise.some((body) => /\bPodRoundPrefix\s*\+/.test(body))).toBe(true);
  const opensAPluginRound = supervise.some((body) => /\bPluginRoundPrefix\s*\+/.test(body));
  const promising = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") &&
        /plugin:<|your process alone|on a different plugin|plugin (update )?(round|task)/i.test(
          readFileSync(file, "utf8").replace(/\s+/g, " ")
        )
    )
    .map((file) =>
      file.startsWith(staged)
        ? path.join("skills", path.relative(staged, file))
        : path.relative(REPO_ROOT, file)
    );
  expect(opensAPluginRound ? [] : promising).toEqual([]);
  if (opensAPluginRound) {
    const escalation = path.join(staged, "legion-controller", "references", "escalation.md");
    const names = /plugin:<daemon contract>/.test(readFileSync(escalation, "utf8"));
    expect(names ? [] : ["skills/legion-controller/references/escalation.md"]).toEqual([]);
  }
});

// The controller's `gh` acts as the review App (LEGION-668): the skill must say how it checks a
// pull request's state, and nothing anywhere in the sweep's roots may still claim the controller
// (or any role) cannot read GitHub. The commands live in the two references SKILL.md sends the
// controller to (its session and its triage), so SKILL.md must link both, and the strings are
// read from those three files only.
test("the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential", () => {
  const controllerDir = path.join(staged, "legion-controller");
  const entry = readFileSync(path.join(controllerDir, "SKILL.md"), "utf8");
  for (const reference of ["session.md", "triage.md"]) {
    expect(entry).toContain(`skill://legion-controller/references/${reference}`);
  }
  const controllerSkill = ["SKILL.md", "references/session.md", "references/triage.md"]
    .map((file) => readFileSync(path.join(controllerDir, file), "utf8"))
    .join("\n");
  expect(controllerSkill).toContain(
    "gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state"
  );
  expect(controllerSkill).not.toContain("gh pr view <url>");
  expect(controllerSkill).toContain("not a pull-request URL");
  expect(controllerSkill).toContain("--label '<each current label>'");
  expect(controllerSkill).toContain("cannot be quoted");
  for (const token of ["OPEN", "MERGED", "CLOSED", "unreadable", '"phase":"controller"']) {
    expect(controllerSkill).toContain(token);
  }

  const forbidden =
    /no GitHub credential|never reads GitHub|has no App|cannot read GitHub|holds no repository credential/;
  const extensions = new Set([".md", ".go", ".ts", ".yaml", ".yml", ".example", ".sh"]);
  const roots = [
    path.join(REPO_ROOT, "skills/legion-controller"),
    path.join(REPO_ROOT, "docs/kubernetes.md"),
    path.join(REPO_ROOT, "packages/daemon/internal"),
    path.join(REPO_ROOT, "docs/site/src/content/docs"),
    path.join(REPO_ROOT, "deploy/kubernetes/daemon/controller.yaml.example"),
    path.join(REPO_ROOT, "AGENTS.md"),
    path.join(REPO_ROOT, "README.md"),
  ];
  const skipDirs = /(^|\/)(node_modules|dist|testdata)(\/|$)/;
  const candidates = roots.flatMap((root) => (statSync(root).isDirectory() ? files(root) : [root]));
  const offending = candidates
    .filter((file) => extensions.has(path.extname(file)) && !skipDirs.test(file))
    .flatMap((file) => {
      const text = readFileSync(file, "utf8");
      if (!forbidden.test(text)) return [];
      return text
        .split("\n")
        .map((line, index) => ({ line, number: index + 1 }))
        .filter(({ line }) => forbidden.test(line))
        .map(({ number }) => `${path.relative(REPO_ROOT, file)}:${number}`);
    });
  expect(offending).toEqual([]);
});

// INFRA-83 took the `CI:` line out of the pull request body, and the forward merge of main put the
// paragraph that defined it back into skills/legion-worker/references/pr-body.md with no conflict
// for anyone to read: a merge keeps both sides by design, so a deletion is undone in silence. The
// body carries no such line, and that reference is the file every worker reads before it touches a
// body, so no staged skill and no daemon prompt may say what the line holds or tell anyone to
// write one.
//
// What it pins is three shapes the regression took, not a class. First, the reference (`CI` line,
// `CI:` line, `**CI**` line, `"CI"` line or bare CI line) as the subject of one of seven verbs
// that say what it contains: `names`, `holds`, `carries`, `lists`, `records`, `shows`, `gives`.
// Second, the reference as the object of one of ten spellings of seven verbs that say to write it
// — `writes`/`written`, `fills`/`filled`, `updates`, `cites`/`cited`, `defines`, `quotes`,
// `records`, each optionally `re-` prefixed — behind a definite determiner (`the`, `a`, `its`,
// `this`, `each`) up to two words after the verb, with a lookbehind that drops `never`, `no
// longer` and `not`. Third, the template's own `**CI:**` field line. The determiner and the
// lookbehind are what let a sentence naming the line while saying it is gone pass: "has no `CI:`
// line", "no `CI:` line to fill", "never writes the `CI` line".
//
// Four shapes it does not catch. The copular: "The body's CI line is the runs GitHub started for
// the code head" uses no verb from either list. The nominalisation: "that reference is the sole
// definition of the CI line", which is the sentence this branch deletes from
// packages/daemon/internal/prompts/roles/implementer.md. A writing verb outside the list: "must
// populate the CI line from one `gh run list`", "Keep the body's CI line current after every
// push". And the anaphora: a sentence saying only "the implementer writes it" passes, which is
// why the anchor is the defining sentence such a paragraph opens with, not its later pronouns.
//
// So this test pins those three shapes and closes no class: a reader who needs a new shape caught
// adds it here rather than assuming it is covered.
test("no staged skill or daemon prompt says what a pull request body `CI` line holds, or tells anyone to write one", () => {
  // `CI` line, `CI:` line, **CI** line, "CI" line, or bare CI line, wrapped across lines or not.
  const reference = String.raw`(?:\*\*|\`|")?CI:?(?:\*\*|\`|")?\s+line`;
  const instructions = [
    // The line as the subject of a verb that says what it contains.
    new RegExp(
      String.raw`${reference}\s+(?:names|holds|carries|lists|records|shows|gives)\b`,
      "gi"
    ),
    // The line as the object of a verb that says to write one, behind a definite determiner so
    // that `no …` and `any …` cannot fill that slot, and not itself negated.
    new RegExp(
      String.raw`(?<!\b(?:never|no longer|not)\s)\b(?:re-)?(?:writes?|written|fills?|filled|updates?|cites?|cited|defines?|quotes?|records?)\s+(?:\w+\s+){0,2}(?:the|a|its|this|each)\s+${reference}`,
      "gi"
    ),
    // The template's own field line, which is an instruction to fill it in.
    /^\*\*CI:?\*\*/gm,
  ];
  const instructing = [staged, promptsRoot]
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) => {
      const text = readFileSync(file, "utf8");
      // A hit in the staged partition reads as its own layout has it
      // (`legion-worker/references/pr-body.md`); a hit in the prompts, which live in the
      // repository, reads from the repository root.
      const name = file.startsWith(staged)
        ? path.relative(staged, file)
        : path.relative(REPO_ROOT, file);
      return instructions
        .flatMap((instruction) => [...text.matchAll(instruction)])
        .map(({ index }) => `${name}:${text.slice(0, index).split("\n").length}`);
    });
  expect(instructing).toEqual([]);
});

// Background work is on for every role: Legion writes no Oh My Pi settings
// overlay, so a long bash call becomes a background job and a `task` subagent runs as one, and a
// role reads a result with the `wait` tool, which a message to its session also returns from. No
// task agent declares `blocking`, and nothing interrupts a turn: a message the daemon sends a role,
// a merge-forward message among them, stops nothing of its. So no skill, no shipped agent and no
// file of the plugin a Claude Code role
// loads may say that a takeover cancels a turn or tell a role to keep its work in
// the foreground, and each that dispatches a task subagent names the tool that reads its result.
//
// Three of the four trees that carry this text are read here — the repository's skills/, the
// plugin's agents/, and a
// fresh `buildLegionPlugin` output, which is generated from those two and so is the only copy of
// `packages/claude-envoy/legion/` there is (nothing commits that tree; the release that
// publishes the plugin builds it in its own build job). The daemon's prompts are the fourth,
// read by
// packages/daemon/internal/prompts/roles_test.go, which walks both of its embed trees and so
// reaches go/claude-mechanics.md, a part `Compose` never joins.
//
// Both halves are exact: the retired sentences as literals, and the dispatch as the boot gate's
// own `task(agent="` against `` `wait` `` as a backticked token, since the English word "wait" is
// in many of these files and one of them forbids waiting (packages/daemon/internal/prompts/go/
// implementer.md, "never wait for CI"). The files that dispatch are listed rather than counted, so
// one that stopped dispatching fails as loudly as one that started, and a reader that found
// nothing cannot pass while checking nothing.
test("no skill, agent or plugin file keeps work in the foreground, and each that dispatches a task names `wait`", () => {
  const retired = [
    "interrupts your turn",
    "tool calls are cancelled",
    "interrupted its turn",
    "run none in the background",
    "one foreground command",
    "blocking: true",
  ];
  const out = path.join(mkdtempSync(path.join(os.tmpdir(), "legion-plugin-")), "build");
  try {
    const built = Bun.spawnSync([process.execPath, PLUGIN_BUILDER, "--out", out]);
    if (built.exitCode !== 0) {
      throw new Error(
        `${PLUGIN_BUILDER} --out ${out} exited ${built.exitCode}:\n${built.stderr.toString()}`
      );
    }
    const plugin = path.join(out, "legion");
    const saidAnyway: string[] = [];
    const dispatching: string[] = [];
    const silent: string[] = [];
    for (const file of [repoSkillsRoot, agentsRoot, plugin].flatMap(files)) {
      const name = file.startsWith(plugin)
        ? `the fresh Claude plugin's ${path.relative(plugin, file)}`
        : path.relative(REPO_ROOT, file);
      const text = readFileSync(file, "utf8");
      for (const phrase of retired) {
        if (text.includes(phrase)) saidAnyway.push(`${name}: ${phrase}`);
      }
      if (text.includes('task(agent="')) {
        dispatching.push(name);
        if (!text.includes("`wait`")) silent.push(name);
      }
    }
    expect(saidAnyway).toEqual([]);
    expect(silent).toEqual([]);
    expect(dispatching.sort()).toEqual([
      "skills/AGENTS.md",
      "skills/dispatch-writing-specs/SKILL.md",
      "skills/legion-architect/SKILL.md",
      "skills/legion-architect/references/decomposition.md",
      "skills/legion-architect/references/design-gate.md",
      "skills/legion-controller/references/consolidation.md",
      "skills/legion-oracle/SKILL.md",
      "skills/legion-retro/SKILL.md",
      "skills/legion-worker/references/merge-gate.md",
      "skills/simplify-settled-diff/SKILL.md",
      "the fresh Claude plugin's skills/legion-architect/SKILL.md",
      "the fresh Claude plugin's skills/legion-architect/references/decomposition.md",
      "the fresh Claude plugin's skills/legion-architect/references/design-gate.md",
      "the fresh Claude plugin's skills/legion-controller/references/consolidation.md",
      "the fresh Claude plugin's skills/legion-oracle/SKILL.md",
      "the fresh Claude plugin's skills/legion-retro/SKILL.md",
      "the fresh Claude plugin's skills/legion-worker/references/merge-gate.md",
      "the fresh Claude plugin's skills/simplify-settled-diff/SKILL.md",
    ]);
  } finally {
    rmSync(path.dirname(out), { recursive: true, force: true });
  }
}, 60_000);

// dispatch://INFRA-332's eight guidance rules, each pinned where it was written. A pin fails when
// a later edit deletes the rule's key sentence; it does not show that an agent follows the rule,
// which the issue's session runs measure. Whitespace is collapsed before the match, as
// shipped-agents.test.ts collapses it, so a pin survives a rewrap of the file it reads.
const collapsed = (file: string) => readFileSync(file, "utf8").replace(/\s+/g, " ");

// A sentence copied into several files is counted once per file, never over a composed prompt,
// where a sentence in both roles/<role>.md and go/worker-common.md would appear twice. A file
// that lost the sentence and a file that gained a second copy both fail, and the files are listed
// rather than counted, so one that stopped carrying it fails as loudly as one that started.
// A `skills/…` path is read from the staged partition, as every other skill pin here is, so what
// is counted is the copy the plugin ships.
function countedOncePerFile(rule: string, relativePaths: string[]) {
  const counts = relativePaths.map((relative) => ({
    file: relative,
    found:
      collapsed(
        relative.startsWith("skills/")
          ? path.join(staged, relative.slice("skills/".length))
          : path.join(REPO_ROOT, relative)
      ).split(rule).length - 1,
  }));
  expect(counts).toEqual(relativePaths.map((file) => ({ file, found: 1 })));
}

// R3: a forward merge brings instances from the base that the branch's own diff never shows, so a
// merge can bring a new instance of a class the branch already answered. The implementer's merge
// procedure carries the class search.
test("a forward merge searches the new head for each earlier finding's class", () => {
  expect(
    collapsed(path.join(staged, "legion-worker", "references", "conflicts-and-rewrites.md"))
  ).toContain(
    "A merge brings instances from the base that the branch's own diff never shows, so before you push the merge search its head for the class of every finding of every earlier review round"
  );
});

// R4: a planner waits on a decision its plan needs rather than completing around it
// (dispatch://INFRA-206). The role prompts carry the rest of R4; roles_test.go pins those.
test("a planner waits on an open decision instead of completing around it", () => {
  expect(collapsed(path.join(staged, "legion-worker", "SKILL.md"))).toContain(
    "a planner on a decision the plan itself needs, because a plan completed around an open decision carries that gap into every later phase"
  );
});

// R5: a review agent returns its whole report, and the reviewer refuses a result without one
// (dispatch://INFRA-331). shipped-agents.test.ts pins the two agent definitions.
test("the reviewer treats a result with no report as a failed run, not a verdict", () => {
  const mergeGate = collapsed(path.join(staged, "legion-worker", "references", "merge-gate.md"));
  expect(mergeGate).toContain(
    "A result that carries no report, or that names another place for it, is a failed run, not a verdict"
  );
  expect(mergeGate).toContain(
    "Reading the agent's diff yourself and recording that in its place is the same as answering the rubric from a bare verdict"
  );
  expect(mergeGate).toContain(
    "that half of the round has no review: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on until it answers"
  );
});

// A review round's pair is recorded as one row per half rather than prose, so a half that did not
// run, or whose findings the reviewer set aside, shows in the body. The reviewer answers a
// `CHANGES` from either half in its review, and approves only when both halves ran at the reviewed
// head or both rows read `not run` for a diff that touches no runtime code.
test("the pair is recorded as a row per half, and the reviewer approves only past both", () => {
  const prBody = collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"));
  expect(prBody).toContain(
    "over <the whole change | <last-reviewed-sha>..<sha>>, one row per half: - `thermonuclear-deep-review`: <APPROVE | CHANGES>, <n> findings stand"
  );
  expect(prBody).toContain(
    "- `thermonuclear-code-quality`: <APPROVE | CHANGES>, <n> findings stand"
  );
  expect(prBody).toContain(
    "each row reads `not run (<the whole change | <last-reviewed-sha>..<sha>> touches no runtime code)`, and the two rows of the last round the pair ran in stay beside them"
  );
  const mergeGate = collapsed(path.join(staged, "legion-worker", "references", "merge-gate.md"));
  expect(mergeGate).toContain(
    "A `CHANGES` from either half is answered in the review: its findings are carried into a `REQUEST_CHANGES`, or each finding is recorded with why it does not stand"
  );
  expect(mergeGate).toContain("Your `APPROVE` needs both halves run at the reviewed head");
  expect(mergeGate).toContain(
    "or both `Thermo` rows reading `not run` because the diff the round reads touches no runtime code"
  );
});

// The pair's own text bounds what it reads and what it reports: a range its task names limits
// what it must read, and a defect it sees in code the pull request changed before that range is
// still a finding, while code the pull request leaves unchanged stays out of its findings, because
// the author fixes only the pull request's diff. Each agent definition is pinned for both halves,
// and each sentence that bounded the findings by the diff the pair was given is pinned absent.
// The daemon's prompts carry the later-round rule; prompts_test.go pins the composed reviewer
// prompt. Every file's gaps are listed together, so a file that lost its sentence fails as loudly
// as the first one checked.
test("the review pair reads the range its task names and reports what the pull request changed", () => {
  const pair = [
    {
      file: "packages/pi-legion/agents/thermonuclear-deep-review.md",
      says: [
        "Do not report issues in code the pull request leaves unchanged, other than the unchanged consumers the rubric's check 2 finds.",
        "A range your task names limits what you must read, not what you report: a defect you see in code the pull request changed before that range is a finding.",
      ],
      retired: ["Do not report unchanged-code issues."],
    },
    {
      file: "packages/pi-legion/agents/thermonuclear-code-quality.md",
      says: [
        "Do not flag code the pull request leaves unchanged.",
        "A range your task names limits what you must read, not what you flag: a smell you see in code the pull request changed before that range is a finding.",
      ],
      retired: ["Do not flag unchanged code."],
    },
  ];
  const scopedToTheRange = pair.flatMap(({ file, says, retired }) => {
    const text = collapsed(path.join(REPO_ROOT, file));
    return [
      ...says.filter((sentence) => !text.includes(sentence)).map((s) => `${file} lacks: ${s}`),
      ...retired.filter((sentence) => text.includes(sentence)).map((s) => `${file} says: ${s}`),
    ];
  });
  expect(scopedToTheRange).toEqual([]);
});

// The last two sentences of each task a reviewer is sent are always the daemon's: the commit's and
// then the entry's, each naming `none` where there is no value for it. The workflow writes them
// on each task of the reviewer it queues (the daemon's namingTheReviewRound), and the claim's send
// writes them, naming `none` for both, on a task no such queue named, a task the release before
// queued or an operator wrote (the daemon's supervise Delivery.message). The task's own text, a
// retry's reason or a title, can hold the same words earlier. So each text that tells the reviewer
// which commit its task names says that the last two are the daemon's and what each `none` means,
// once: the daemon part, the role part and the core part of the reviewer's prompt.
const reviewerTexts = [
  "packages/daemon/internal/prompts/go/reviewer.md",
  "packages/daemon/internal/prompts/roles/reviewer.md",
  "packages/daemon/internal/prompts/roles/core/reviewer.md",
];
test("each reviewer text says the last two sentences of each task are always the daemon's", () => {
  countedOncePerFile(
    "The last two sentences of each task you are given are always the daemon's: `Last reviewed commit: <sha>.` or `Last reviewed commit: none.`, then `Review entry: <n>.` or `Review entry: none.`. `Last reviewed commit: none.` means this task names no commit, so you read the whole change. `Review entry: none.` means this task names no entry, so it does not change the entry of your round. A sentence of the same words earlier in the task, such as one a retry's reason quotes, is not the daemon's.",
    reviewerTexts
  );
});

// R6: a pipe's status is the last command's, so `$?` after `cmd | tail` reads the pager's
// (dispatch://INFRA-334). The sentence goes in the always-loaded prompts and agent definitions,
// because skills/legion-worker/references/tools.md loads only on a tool or test question.
test("every shipped prompt and bash-running agent says how to read a piped command's status", () => {
  countedOncePerFile(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: `${PIPESTATUS[0]}` is the shell expansion the rule teaches, quoted verbatim.
    "Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a subshell of that one line; never set `-e`, `-u` or `pipefail` at the top level of your shell, where under `pipefail` a later `cmd | head` exits 141 and reads as a failure although it did its job, and under `-e` or `-u` a later command that fails or reads an unset variable ends the call there. Never pipe a write — a `dispatch` command that records something, or any command whose own output tells you what it did — into `head`, `tail` or `grep`: the status you read would be the pager's, and the line that says what happened is the line you cut.",
    [
      "packages/daemon/internal/prompts/go/worker-common.md",
      "packages/daemon/internal/prompts/go/architect-common.md",
      "packages/daemon/internal/prompts/go/controller-headless.md",
      "packages/daemon/internal/prompts/go/controller-interactive.md",
      "packages/pi-legion/agents/deep-worker.md",
      "packages/pi-legion/agents/plan-gap-analyst.md",
      "packages/pi-legion/agents/plan-reviewer.md",
      "packages/pi-legion/agents/thermonuclear-deep-review.md",
      "packages/pi-legion/agents/thermonuclear-code-quality.md",
    ]
  );
});

// R8: every role of an issue shares one $TMPDIR on the issue's volume, so a glob sweep deletes
// another role's open directory (dispatch://INFRA-654). The reviewer's pair states it in
// `thermonuclear-code-quality`, which the Envoy plugin now ships and whose guard holds it there.
test("every prompt and agent that may write says to make its own scratch and delete only what it made", () => {
  countedOncePerFile(
    'Make your own scratch with `mktemp -d` and delete only the paths you made, each by its exact path. Never delete by a glob in a shared directory: every role of this issue writes into the same `$TMPDIR` on the issue\'s volume, so a sweep like `rm -rf "$TMPDIR"/tmp.*` deletes directories another role has open.',
    [
      "packages/pi-legion/agents/deep-worker.md",
      "packages/pi-legion/agents/thermonuclear-deep-review.md",
      "packages/pi-legion/agents/thermonuclear-code-quality.md",
      "packages/daemon/internal/prompts/go/worker-common.md",
      "packages/daemon/internal/prompts/go/architect-common.md",
    ]
  );
});

// R9: a push moves the branch while the pull request can keep naming the old commit, so the new
// commit gets no checks. The daemon's worker prompt holds the step, each sentence counted once, so
// the pinned sentences cannot drift. The carve-out that names the step's reads as the one sleep
// the "never sleep in a loop" rule allows is held in the implementer's prompt part, and the
// reviewer's rule that it pushes nothing keeps the step from falling to it. This shows the text
// ships, never that a role follows it.
test("the worker prompt tells a role to read the pull request's head after its own push", () => {
  const workerPrompt = ["packages/daemon/internal/prompts/go/worker-common.md"];
  const step = [
    "gh pr view <n> --repo <the issue's repository> --json headRefOid --jq .headRefOid",
    "about ten seconds after your push, again about ten seconds later, and a last time about thirty seconds after that, with a read that gives the pushed commit ending the step there and only the last read deciding a mismatch",
    "This read is of the pull request's own head and of no check, so the wait it makes is not a wait on CI",
    "an empty one when your work is complete",
    "The empty commit changes no file, so record each `before_merge` check again at the new head as carried from the commit before it (`skill://legion-worker/references/pr-body.md#what-a-result-is`), and run none of them again. A check carrying `ci_check` is CI's, so skip it.",
    "tell your architect the commit you pushed, the head each read gave and the time of each, and end the turn with a WAITING line naming that answer rather than completing: a phase completed here hands the next role a commit the pull request does not name.",
    "The push you make to answer an update message is the exception: that message has you push, let your subagents and background jobs finish or stop them, push what they made, answer the daemon and end the turn, so make this read in your next turn, before your phase completes.",
    "The merger pushes nothing, so this step never falls to it; it reports a mismatch it sees to the architect.",
    "the empty commit that carries the pull request onto the commit you pushed",
  ];
  for (const rule of step) {
    countedOncePerFile(rule, workerPrompt);
  }
  countedOncePerFile(
    "the step's spaced reads of the pull request's head after a push are the one sleep this rule allows, and they read no check",
    ["packages/daemon/internal/prompts/go/implementer.md"]
  );
  countedOncePerFile(
    "`jj -R \"$LEGION_WORKSPACE\" new --no-edit --insert-after @- -m '<issue>: carry the pull request to the pushed commit'`",
    workerPrompt
  );
  countedOncePerFile(
    '`jj -R "$LEGION_WORKSPACE" log -r legion/<issue>@origin --no-graph -T commit_id`',
    workerPrompt
  );
  countedOncePerFile("Create no commit and push nothing", [
    "packages/daemon/internal/prompts/roles/reviewer.md",
  ]);
});

// A check carrying `ci_check` is proved by the CI run it names, so no role runs it or records its
// result, and `dispatch results` derives that result. The worker prompt's copy of the push step
// carries the exception in one phrase, counted once. The role prompt parts are pinned in
// roles_test.go `TestEveryRolePartCarriesItsGuidanceRule`.
test("the worker prompt's push step leaves out a check CI proves", () => {
  countedOncePerFile("check carrying `ci_check` is CI's", [
    "packages/daemon/internal/prompts/go/worker-common.md",
  ]);
});

// A `ci_check` that no run of its name answers is a defect of the check's words, which the
// architect corrects, and no role sends that to an implementer who cannot cure it
// (`ci-checks.md`, "A name that no run answers"). Retro's step that sends a CI check with no pass
// back names that case and gives it to the architect, so the two texts say the same thing
// (dispatch://INFRA-3204, tester round 5).
test("retro gives a ci_check that no run answers to the architect, not to the implementer", () => {
  const retro = collapsed(path.join(staged, "legion-retro", "SKILL.md"));
  const sentences = retro.split(/(?<=\.)\s+/);
  const absent = sentences.filter((sentence) => sentence.includes("`absent`"));
  expect(absent.length).toBeGreaterThan(0);
  expect(absent.some((sentence) => sentence.includes("architect"))).toBe(true);
  expect(absent.some((sentence) => sentence.includes("to: implementing"))).toBe(false);
});

// dispatch://INFRA-924's rule, beside dispatch://INFRA-332's eight above: a retro learning cites
// a file and a symbol, or text quoted from it, read at the head the pull request commits, never a
// line number the next edit above it moves.
test("a retro learning cites a symbol or quoted text at the committed head, never a line number", () => {
  const learnings = collapsed(path.join(staged, "legion-retro", "references", "learnings.md"));
  expect(learnings).toContain(
    "**The evidence** below the rule: the file and symbol, or text quoted from the file, read at the head you commit, and what went wrong before the rule."
  );
  expect(learnings).toContain(
    "Never a line number: the next edit above it moves it. Where the head no longer shows what your sentence says, quote the text and name the commit you read it at."
  );
  expect(learnings).toContain(
    "Evidence: <file and symbol, or text quoted from it, of this pull request, and what went wrong>."
  );
  countedOncePerFile(
    "Drop a point that cites no file, symbol, quoted text or commit of this pull request",
    ["skills/legion-retro/references/learnings.md", "skills/legion-retro/SKILL.md"]
  );
  // The two files this change touches, named rather than swept: three other staged skills carry
  // these strings, in six files, in finding formats the specification scopes out
  // (dispatch://INFRA-924).
  const retired = ["file, line or commit", "file:line", "file and line"];
  const retroFiles: [string, string][] = [
    ["references/learnings.md", learnings],
    ["SKILL.md", collapsed(path.join(staged, "legion-retro", "SKILL.md"))],
  ];
  const carried = retroFiles.flatMap(([file, text]) =>
    retired.filter((phrase) => text.includes(phrase)).map((phrase) => `${file}: ${phrase}`)
  );
  expect(carried).toEqual([]);
});

// The completion that ends the retro rates the plan, and this skill's step that gives the two
// fields is the one place the rule of the rating is stated in prose: the implementer prompt sends
// the agent here (prompts_test.go), and the daemon refuses a completion that breaks the rule
// (the daemon's planRatingOf and planRatingRefusal). So the step names the two
// fields, the three codes the daemon refuses with, and the scale and the bound of the reason as the
// contract holds them: a new scale or bound fails here until the skill states it. The bound is
// pinned with the space before it, since step 5's "32,000 characters" holds "2,000 characters"
// without it. This shows the text ships, never that a role follows it.
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
