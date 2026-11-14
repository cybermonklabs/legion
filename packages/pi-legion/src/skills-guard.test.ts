import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
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
// task agent declares `blocking`, and nothing interrupts a turn: a CI red that ends a
// phase starts the implementer at once and tells the role whose phase ended by one message, which
// stops nothing of its. So no skill, no shipped agent and no file of the plugin a Claude Code role
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

// R1: the simplify pass covers every file of the diff, docs-only diffs included, and a defect it
// sees is reported rather than dropped as out of scope (dispatch://INFRA-198).
test("the simplify pass reads every file of its scope and reports the defects it sees", () => {
  const simplify = collapsed(path.join(staged, "simplify-settled-diff", "SKILL.md"));
  expect(simplify).toContain(
    "Every file of the scope is in the pass: documentation, workflow and configuration files as much as code, and a scope that holds no code at all still runs."
  );
  expect(simplify).toContain("A defect you see is reported, never dropped.");
  expect(simplify).not.toContain("For mixed scopes, retain only the code.");
  const prBody = collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"));
  expect(prBody).toContain("whatever the diff touches, a docs-only diff included.");
  expect(prBody).toContain(
    "The pass covers every file of the diff, workflow YAML and documentation with the code, and a defect it reports is fixed outside the pass with the results recorded again, never dropped as out of scope; the `Thermo` line's `defects reported` half names how many and the commit that fixed them"
  );
  // The Thermo line is written on every diff now, and carries what the pass reported; only its
  // pair half is omitted on a docs-only one, which leaves the reviewer's pair where it was.
  expect(prBody).toContain("defects reported: <none | <n>, fixed in <sha>>");
  expect(prBody).toContain(
    "(on a docs-only PR the simplify half and the defects are recorded as on any other diff, and the pair half reads `thermonuclear pair: not run (docs-only)`)"
  );
  expect(prBody).not.toContain("a docs-only diff gets none");
  const mergeGate = collapsed(path.join(staged, "legion-worker", "references", "merge-gate.md"));
  expect(mergeGate).toContain(
    "On a docs-only PR the `Thermo` line still records the simplify pass and the defects it reported, and its pair half reads `thermonuclear pair: not run (docs-only)`."
  );
  expect(mergeGate).not.toContain("Skip the `Thermo` line entirely on a docs-only PR.");
});

// R2: a check that cannot fail is not a result, and a documented procedure counts only when it
// ran (dispatch://INFRA-142, dispatch://INFRA-198).
test("a check that cannot fail is not a result, and a documented procedure counts only when it ran", () => {
  const prBody = collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"));
  expect(prBody).toContain("**A check that cannot fail is not a result.**");
  expect(prBody).toContain(
    "A documented procedure counts only when it ran: reading it, or reading the document that describes it, is not a run"
  );
});

// R3: the unchanged-diff fingerprint covers the branch's own diff alone, so a forward merge can
// bring a new instance of a class the branch already answered (dispatch://INFRA-83). The `E2E`
// re-check wording carries the class search in both files that hold it.
test("the forward-merge re-check searches the new head for each earlier finding's class", () => {
  expect(
    collapsed(path.join(staged, "legion-worker", "references", "conflicts-and-rewrites.md"))
  ).toContain(
    "The fingerprint answers one question only: whether the branch's own diff changed. It cannot show an instance the merge brought in from the base"
  );
  expect(collapsed(path.join(staged, "legion-architect", "references", "acceptance.md"))).toContain(
    "the tester re-runs the bare gates only and the class search of every earlier round's findings at the new head;"
  );
  countedOncePerFile(
    "forward-merge re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only, class search of earlier findings clean",
    [
      "skills/legion-worker/references/pr-body.md",
      "packages/daemon/internal/prompts/roles/tester.md",
    ]
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

// R6: a pipe's status is the last command's, so `$?` after `cmd | tail` reads the pager's
// (dispatch://INFRA-334). The sentence goes in the always-loaded prompts and agent definitions,
// because skills/legion-worker/references/tools.md loads only on a tool or test question.
test("every shipped prompt and bash-running agent says how to read a piped command's status", () => {
  countedOncePerFile(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: `${PIPESTATUS[0]}` is the shell expansion the rule teaches, quoted verbatim.
    "Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a subshell of that one line; never set `pipefail` at the top level of your shell, where a later `cmd | head` exits 141 and reads as a failure although it did its job. Never pipe a write — a `dispatch` command that records something, or any command whose own output tells you what it did — into `head`, `tail` or `grep`: the status you read would be the pager's, and the line that says what happened is the line you cut.",
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
// commit gets no checks (dispatch://INFRA-1318). The step is one wording held across
// `skill://legion-worker` and the daemon's worker prompt, counted once in each file, so the
// pinned sentences cannot drift; the prompt also carries text of its own that no pin here holds,
// and the pins below hold what only the prompt carries and the skill points at. The carve-out
// that names the step's reads as the one sleep the "never sleep in a loop" rule allows is a
// second wording, held across the pr-body reference and the implementer's prompt part. The three
// members the step added to lists elsewhere in these texts are pinned too, since a list no test
// reads loses its member silently. This shows the text ships, never that a role follows it.
test("the worker skill tells a role to read the pull request's head after its own push", () => {
  const sharedWithTheWorkerPrompt = [
    "skills/legion-worker/SKILL.md",
    "packages/daemon/internal/prompts/go/worker-common.md",
  ];
  const step = [
    "gh pr view <n> --repo <the issue's repository> --json headRefOid --jq .headRefOid",
    "about ten seconds after your push, again about ten seconds later, and a last time about thirty seconds after that, with a read that gives the pushed commit ending the step there and only the last read deciding a mismatch",
    "This read is of the pull request's own head and of no check, so the wait it makes is not a wait on CI",
    "an empty one when your work is complete",
    "A new head is a different commit, so record each `before_merge` check again at the new head. A check carrying `ci_check` is CI's, so skip it.",
    "tell your architect the commit you pushed, the head each read gave and the time of each, and end the turn with a WAITING line naming that answer rather than completing: a phase completed here hands the next role a commit the pull request does not name.",
    "The push you make to answer an update message is the exception: that message has you answer the daemon and end the turn at once, so make this read in your next turn, before your phase completes.",
    "The merger pushes nothing, so this step never falls to it; it reports a mismatch it sees to the architect.",
    "the empty commit that carries the pull request onto the commit you pushed",
  ];
  for (const rule of step) {
    countedOncePerFile(rule, sharedWithTheWorkerPrompt);
  }
  countedOncePerFile(
    "the step's spaced reads of the pull request's head after a push are the one sleep this rule allows, and they read no check",
    [
      "skills/legion-worker/references/pr-body.md",
      "packages/daemon/internal/prompts/go/implementer.md",
    ]
  );
  const onlyInTheWorkerPrompt = ["packages/daemon/internal/prompts/go/worker-common.md"];
  countedOncePerFile(
    "`jj -R \"$LEGION_WORKSPACE\" new --no-edit --insert-after @- -m '<issue>: carry the pull request to the pushed commit'`",
    onlyInTheWorkerPrompt
  );
  countedOncePerFile(
    '`jj -R "$LEGION_WORKSPACE" log -r legion/<issue>@origin --no-graph -T commit_id`',
    onlyInTheWorkerPrompt
  );
  countedOncePerFile("Three waits stand rather than proceed", ["skills/legion-worker/SKILL.md"]);
  countedOncePerFile("the empty commit that carries it there", [
    "packages/daemon/internal/prompts/roles/reviewer.md",
  ]);
  expect(collapsed(path.join(staged, "legion-worker", "SKILL.md"))).toContain(
    "When the last read still is not your commit, carry the pull request with a commit of its own, an empty one when your work is complete and an empty one just as much when `@` holds work of yours, since a push of a commit the branch already holds moves no ref; the push section of your role prompt gives the one `jj` recipe, which leaves `@` and everything in it where it is."
  );
});

// dispatch://INFRA-3204: a check carrying `ci_check` is proved by the CI run it names, so no role
// runs it or records its result, and `dispatch results` derives that result. The worker skill's
// page holds the whole rule, and each skill text that orders a check run, recorded or judged, or
// lists a check's attributes, carries the exception in one phrase, counted once per file, as does
// the worker prompt's copy of the push step above. The role prompt parts are pinned in
// roles_test.go `TestEveryRolePartCarriesItsGuidanceRule`, and `dispatch-writing-specs` in the
// Envoy plugin's guard.
test("each text that orders a check run or recorded leaves out a check CI proves", () => {
  countedOncePerFile("check carrying `ci_check` is CI's", [
    "skills/legion-worker/references/ci-checks.md",
    "skills/legion-worker/SKILL.md",
    "skills/legion-worker/references/pr-body.md",
    "skills/legion-worker/references/merge-gate.md",
    "skills/legion-retro/SKILL.md",
    "skills/legion-architect/SKILL.md",
    "skills/legion-architect/references/design-gate.md",
    "packages/daemon/internal/prompts/go/worker-common.md",
  ]);
});

// The same rule where a text orders a check's evidence judged or recorded. Retro's trigger is met
// by a derived result and names R5's three cases: a derived fail, or a run that concluded and
// proved nothing, goes back to implementing; a run not concluded is waited for; and a name no run
// answers is the architect's finding. The architect's last round records only the checks with no
// `ci_check`, and the tester reads a `ci_check` job as that check's evidence, which its prompt's
// core part says too.
test("a derived result satisfies the passages that judge or record a check's evidence", () => {
  const retro = collapsed(path.join(staged, "legion-retro", "SKILL.md"));
  expect(retro).toContain(
    "A `ci_check` check has no pass standing at that head when its derived result is a fail, or its run concluded `skipped`, `neutral` or `cancelled`."
  );
  expect(retro).toContain(
    "Wait for a run that has not concluded, and read again where GitHub could not be read."
  );
  expect(retro).toContain("Neither wait nor that finding moves the issue back.");
  expect(collapsed(path.join(staged, "legion-architect", "references", "acceptance.md"))).toContain(
    "run each check with no `ci_check` on the default branch and record each result"
  );
  expect(collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"))).toContain(
    "recorded for no later step, except a `ci_check` job"
  );
  expect(collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"))).toContain(
    "A name no run answers once the head's checks have settled goes to the architect as a finding."
  );
  expect(
    collapsed(path.join(staged, "legion-worker", "references", "conflicts-and-rewrites.md"))
  ).toContain(
    "record the issue's checks again at the merge head, each check with no `ci_check` (a check carrying one is read again from its CI run)"
  );
});

// The architect writes a check as `ci_check` where a CI job on an ordinary pull request already
// makes its proof, so the mechanism is used rather than only available (R5).
test("the architect writes a check CI already proves as ci_check", () => {
  expect(
    collapsed(path.join(staged, "legion-architect", "references", "design-gate.md"))
  ).toContain(
    "Where a CI job that runs on an ordinary pull request already makes a check's proof, write that check as `ci_check` rather than as a command."
  );
  expect(collapsed(path.join(staged, "legion-architect", "SKILL.md"))).toContain(
    "Where a CI job that runs on an ordinary pull request already makes a check's proof, write `ci_check` rather than a command."
  );
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

// dispatch://INFRA-3116's rule, beside the rules above: where a tree builds or changes a surface a
// person opens, the architect shows that person the surface and gets their answer before it asks
// them to approve the root spec. The rule is one bullet of section 1 and the section of the
// design-gate reference that bullet sends the architect to, and each pin here is one of its
// sentences, read from the staged partition. A pin fails when a later edit drops or rewords the
// sentence, so the rule cannot leave the text a Legion pane loads in silence; none of them shows
// that an architect in a live tree runs the round, which only that tree's own spec, its pictures
// and the person's answer show.
test("the architect shows a person the surface before it asks them to approve the spec", () => {
  const skill = collapsed(path.join(staged, "legion-architect", "SKILL.md"));
  const gate = collapsed(path.join(staged, "legion-architect", "references", "design-gate.md"));
  expect(skill).toContain(
    '**Show the surface before you ask.** Where this tree builds or changes a surface a person opens — a page, a screen, a dashboard — that person sees it and answers before you ask them to approve the spec, because they refuse a spec for a surface they have not seen. Whether a change needs that is your judgement, and a large visual change wants a page they can click rather than a picture. The readings, the round and where the pictures end up: `skill://legion-architect/references/design-gate.md`, "Show the surface before you ask". A change whose only reader surface is text carries that literal text in the spec instead; a change with no reader surface carries neither.'
  );
  expect(gate).toContain("## Show the surface before you ask");
  expect(gate).toContain(
    "Where this tree builds or changes a surface a person opens — a page, a screen, a dashboard — that person sees it and answers before you ask them to approve the spec. A spec for a surface they have not seen is refused, and the refusal costs a spec version, a closed gate, a second request and their time twice."
  );
  // The four readings that inform the judgement, each whole: a reading that lost its carve-out
  // would send a round for every label change, or none for a new page.
  expect(gate).toContain(
    "Whether a change needs the round is your judgement. These readings inform it:"
  );
  expect(gate).toContain(
    "A new surface a person opens, and a large visual change to one, need it. The larger the visual change, the more a page the person can click is worth over a picture: a person judges a layout from a picture, and judges a flow only by acting on it."
  );
  expect(gate).toContain(
    "A small change inside a shape that stands — a label, a copy change, a style fix, a bug fix — does not, unless you are unsure what the person wants, which is itself a reason to show them."
  );
  expect(gate).toContain(
    "A change whose only reader surface is text — an alert message, a report, a command's printed output — carries that literal text in the spec, at the values it will carry, and needs no round. The person reads the spec before the build; a pull request body reaches a reviewer after it."
  );
  expect(gate).toContain(
    "A change with no surface a person reads needs nothing, and nothing in this section applies to it."
  );
  // The round and its five steps. A step's collapsed string carries the fenced block inside it,
  // so the `chromium` recipe, the `dispatch artifact` upload and the picture-link form of a
  // caption are pinned with the prose that introduces them.
  expect(gate).toContain(
    "The round is standard user research run on one reader: show the surface rather than describe it, let them act on it where the flow is the question, ask what they expected rather than whether they approve, and show another round on an answer that asks for changes (https://www.nngroup.com/articles/ux-prototype-hi-lo-fidelity)."
  );
  expect(gate).toContain("Before you first ask the person to approve the spec:");
  expect(gate).toContain(
    "Build the surface as a page, from real readings of the real system. Draw the empty state and the worst realistic data, or say which states you did not draw. Mark every invented figure in the picture itself, the way a check names its negative control: a picture whose figures are all plausible is the hardest one to judge. Where the surface exists already, draw it as it is now beside the new one: a picture a person cannot place is a round spent for nothing."
  );
  expect(gate).toContain(
    "Take the picture with the pod's own Chromium, then put each one on the issue: ```bash chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \\ --screenshot=<file.png> file://<page.html> dispatch artifact --issue <KEY> --name <name>.png --path <file.png> ``` The upload stores it as an `image` artifact and prints its slug."
  );
  expect(gate).toContain(
    "For a large visual change, or where the question is about a flow rather than a layout, build the page so the person can click it, put that one file up beside the pictures as a `file` artifact, and say in your question what works in it and what does not."
  );
  expect(gate).toContain(
    "Ask one question with two to four answers, one of which says this is the wrong surface. Write no code on the branch for the round, and end your turn as for any question to a person."
  );
  expect(gate).toContain(
    "The agreed pictures go in the spec, in line, each with a line of prose under it saying what it shows and which of its readings are real: ```md ![the priorities page with no priority set](dispatch://<KEY>/artifact/<slug>) The page as a reader opens it. The 529 issues and their ages are read from this deployment; the two ranks on the right are invented. ```"
  );
  expect(gate).toContain(
    "One further line says what the pictures do not show: the states you did not draw."
  );
  expect(gate).toContain(
    "The words of the spec still govern the build, because a reader on the command line gets the link text and not the picture."
  );
  expect(gate).toContain(
    "You end the round, not the person: when their answer is carried into the pictures or into the spec, go on to the approval request with the pictures as they then stand. A person who stops answering holds nothing up."
  );
  expect(gate).toContain(
    "The picture is the cheapest thing that answers the question. A labelled sketch is enough where it answers it. Where the real surface, built read-only, costs no more than the mock-up, build that and show that."
  );
  // The six ordering lines, each whole: they are what keeps the rule from closing an open gate or
  // writing a spec version of its own, so a dropped one holds a tree's merge on a new version.
  expect(gate).toContain(
    "A spec written before the picture is fine. What may not happen is reaching the gate with no picture."
  );
  expect(gate).toContain(
    'A tree whose gate is already open starts no round for the work in flight and writes no spec version for a picture: a new version closes the gate and holds the merge until a person approves it ("After approval").'
  );
  expect(gate).toContain(
    "The round writes no spec version of its own. The agreed pictures go into the version you first submit for approval."
  );
  expect(gate).toContain(
    'A picture that must change after approval follows "After approval": it is a new version only when what is built changes, and then you request approval again.'
  );
  expect(gate).toContain(
    "Where the `Design gate steps` sentence of your `Legion addressing` line says `none`, nobody approves the spec. Make the picture, put it in the spec for whoever builds it, and ask nobody."
  );
  expect(gate).toContain(
    "The surfaces this tree's children build are yours. Their pictures go in the root spec, and a sub-architect that finds a surface the root spec never drew tells the root architect."
  );
});
