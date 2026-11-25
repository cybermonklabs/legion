import { describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { SessionMessage } from "../skill-scenarios/transcript";
import { type Arm, buildArm } from "./arms";
import { type Case, pathHash } from "./cases";
import { Refused, readFrontmatter } from "./common";
import {
  type AgentDefinition,
  buildContext,
  type ContextOptions,
  cacheKey,
  ghSource,
  gitBlobId,
  IncompleteCacheEntry,
  installedCopies,
  LOCAL_SKILL_REPO,
  legionRole,
  localSource,
  mustRun,
  NoSuchFile,
  PROMPTS_GO,
  type RepoSource,
  skillFrontmatter,
  subagentDefinition,
  UnreadableDirectory,
} from "./context";

// The case context (context.ts) built from made-up repositories, read from local
// directories in place of `gh`: `acme/skills` (the skills alpha and beta), `acme/project` (an
// AGENTS.md and a CLAUDE.md at its root, Claude Code's user memory at .claude/CLAUDE.md, and the
// AGENTS.md, .claude/CLAUDE.md and .github/copilot-instructions.md of its directory agents/vm),
// `acme/infra` (a project's instructions file) and `acme/legion`, which holds a
// made-up daemon whose role prompt composer has the API of packages/daemon/internal/prompts
// (`New(stateDir)`, `Compose(role, isRoot)`, the `roles/` parts snapshotted under
// prompts/shared and the `go/` parts under prompts/go) and two made-up plugins. The Legion row
// runs `go test` in a copy of that repository; a run without Go skips it, except on GitHub
// Actions, where the pi-legion job sets Go up.
const COMMIT = "89abcdef0123456789abcdef0123456789abcdef";
const goMissing = Bun.which("go") === null;
const onActions = process.env.GITHUB_ACTIONS === "true";
if (goMissing && !onActions) {
  console.log("context.test.ts: no `go` on PATH, so the Legion role prompt row is skipped");
}

const dir = temporaryDirectory("skill-cuts-context", { real: true });

/** Writes `files` (path to text) under `root`. */
function writeTree(root: string, files: Record<string, string>): void {
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    writeFileSync(path.join(root, file), text);
  }
}

const ALPHA =
  "---\nname: alpha\ndescription: >-\n  Use when the alpha list\n  is asked for.\n---\n\n# Alpha\n\n1. One.\n";
const ALPHA_ARM = ALPHA.replace("1. One.", "1. One, from the arm.");
const BETA = "---\nname: beta\ndescription: Use for the beta greeting.\n---\n\n# Beta\n";
const AGENTS = "# Project\n\nKeep answers short.\n";
/** Claude Code's user memory in a made-up repository: a devbox profile's user-level file. */
const USER_RULES = "# Made-up user rules\n\nAnswer in one line.\n";
/** A made-up repository's root CLAUDE.md: the project context file of a devbox session whose
 *  working directory is a checkout of it. */
const PROJECT_RULES = "# Made-up project rules\n\nName each file you change.\n";
/** The made-up acme/project's context files of its directory agents/vm. */
const VM_AGENTS = "# VM\n\nRun the VM steps in order.\n";
const VM_CLAUDE = "# VM for Claude\n\nKeep the VM notes short.\n";
const VM_COPILOT = "# VM for Copilot\n\nName the VM in each reply.\n";
const INSTRUCTIONS = "# Acme instructions\n\nWork the acme way.\n\n\n";

/** The made-up daemon module: its role prompt composer and the parts it embeds. */
const DAEMON: Record<string, string> = {
  "packages/daemon/go.mod": "module example.com/acme/daemon\n\ngo 1.22\n",
  "packages/daemon/internal/claim/claim.go": `package claim

type Role string

const (
	RoleArchitect   Role = "architect"
	RoleImplementer Role = "implementer"
)
`,
  [PROMPTS_GO]: `package prompts

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"example.com/acme/daemon/internal/claim"
)

//go:embed roles
var roleParts embed.FS

//go:embed go/*.md
var goParts embed.FS

type Parts struct{ RolePromptPaths []string }

type Composer struct{ sharedDir, goDir string }

func write(parts embed.FS, dir, to string) error {
	return fs.WalkDir(parts, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := parts.ReadFile(name)
		if err != nil {
			return err
		}
		target := filepath.Join(to, name[len(dir)+1:])
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
}

func New(stateDir string) (*Composer, error) {
	shared := filepath.Join(stateDir, "prompts", "shared")
	goDir := filepath.Join(stateDir, "prompts", "go")
	if err := write(roleParts, "roles", shared); err != nil {
		return nil, err
	}
	if err := write(goParts, "go", goDir); err != nil {
		return nil, err
	}
	return &Composer{sharedDir: shared, goDir: goDir}, nil
}

func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	switch {
	case role == claim.RoleArchitect && isRoot:
		return Parts{RolePromptPaths: []string{filepath.Join(c.sharedDir, "architect-root.md"), filepath.Join(c.goDir, "architect.md")}}, nil
	case role == claim.RoleImplementer:
		return Parts{RolePromptPaths: []string{filepath.Join(c.sharedDir, "core", "common.md"), filepath.Join(c.sharedDir, "implementer.md"), filepath.Join(c.goDir, "worker.md")}}, nil
	}
	return Parts{}, fmt.Errorf("unsupported role %q", role)
}

func (c *Composer) ControllerPromptPaths(headless bool) ([]string, error) {
	launch := "controller-interactive.md"
	if headless {
		launch = "controller-headless.md"
	}
	return []string{filepath.Join(c.sharedDir, "controller-root.md"), filepath.Join(c.goDir, launch)}, nil
}
`,
  "packages/daemon/internal/prompts/roles/core/common.md": "Core part.\n",
  "packages/daemon/internal/prompts/roles/implementer.md": "Implementer part.\n",
  "packages/daemon/internal/prompts/roles/architect-root.md": "Root architect part.\n",
  "packages/daemon/internal/prompts/roles/controller-root.md": "Controller part.\n",
  "packages/daemon/internal/prompts/go/worker.md": "Worker part.\n\n",
  "packages/daemon/internal/prompts/go/architect.md": "Architect part.\n",
  "packages/daemon/internal/prompts/go/controller-headless.md": "Headless controller part.\n",
  "packages/daemon/internal/prompts/go/controller-interactive.md": "Interactive controller part.\n",
};

/** A made-up plugin that builds a bundle, ships `dist`, and names `dist/skills` for its skills,
 *  and one whose build fails. */
const PLUGINS: Record<string, string> = {
  "packages/plugin-a/package.json": JSON.stringify({
    name: "plugin-a",
    files: ["dist"],
    scripts: { build: "bun build extensions/a.ts --outdir dist --target bun" },
    omp: { extensions: ["extensions/a.ts"], skills: ["dist/skills"] },
  }),
  "packages/plugin-a/extensions/a.ts": "export default function plugin(): void {}\n",
  "packages/plugin-a/dist/skills/stale/SKILL.md":
    "---\nname: stale\ndescription: A stale copy.\n---\n",
  "packages/plugin-b/package.json": JSON.stringify({
    name: "plugin-b",
    files: ["dist"],
    scripts: { build: "exit 3" },
    omp: { extensions: ["extensions/b.ts"] },
  }),
  "packages/plugin-b/extensions/b.ts": "export default function plugin(): void {}\n",
};

/** Writes the made-up repositories under `root`. */
function writeRepositories(root: string): void {
  writeTree(path.join(root, "acme/skills", COMMIT), {
    "skills/alpha/SKILL.md": ALPHA,
    "skills/beta/SKILL.md": BETA,
    "skills/beta/references/r.md": "Beta reference.\n",
  });
  writeTree(path.join(root, "acme/project", COMMIT), {
    "AGENTS.md": AGENTS,
    "CLAUDE.md": PROJECT_RULES,
    ".claude/CLAUDE.md": USER_RULES,
    "agents/vm/AGENTS.md": VM_AGENTS,
    "agents/vm/.claude/CLAUDE.md": VM_CLAUDE,
    "agents/vm/.github/copilot-instructions.md": VM_COPILOT,
  });
  writeTree(path.join(root, "acme/infra", COMMIT), {
    "legion/instructions/acme.md": INSTRUCTIONS,
  });
  writeTree(path.join(root, "acme/legion", COMMIT), {
    "package.json": JSON.stringify({ name: "acme-legion", private: true }),
    ...DAEMON,
    ...PLUGINS,
    "plugins/skills-only/skills/beta/SKILL.md": BETA,
  });
}

/** A read of skill://alpha and its result, then the step. */
const messages: SessionMessage[] = [
  { role: "user", content: [{ type: "text", text: "Write the alpha list." }] },
  {
    role: "assistant",
    content: [
      { type: "toolCall", id: "toolu_1", name: "read", arguments: { path: "skill://alpha" } },
    ],
  },
  {
    role: "toolResult",
    toolCallId: "toolu_1",
    toolName: "read",
    content: [{ type: "text", text: `[Skill file: /skills/alpha/SKILL.md]\n${ALPHA.trimEnd()}` }],
    isError: false,
  },
];

/** A case of alpha in `context`, its profile naming `plugins` and `contextFiles`. */
function aCase(context: string, profile: Partial<Case["profile"]>): Case {
  return {
    id: `case-${context}`,
    cut: "acme-skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: "1. One.", lineNumber: 9, lineCount: 9 },
    criterion: { pass: "It lists one.", fail: "It lists none." },
    model: "anthropic/claude-sonnet-5-5",
    thinking: "off",
    context,
    profile: {
      skills: [
        { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" },
        { repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" },
      ],
      plugins: [],
      contextFiles: [{ repo: "acme/project", commit: COMMIT, path: "AGENTS.md" }],
      ...profile,
    },
    messages,
    skillReads: [2],
    realStep: { role: "assistant", content: [{ type: "text", text: "1. One." }] },
    violatingStep: { role: "assistant", content: [{ type: "text", text: "None." }] },
    source: { store: "store_a", pathHash: pathHash("/sessions/a.jsonl") },
    sessionStart: "2026-11-20T01:00:00Z",
    secretsRemoved: 0,
    sessionDigest: null,
  };
}

/** The options and the old arm of `c`, under `root`. */
function setUp(root: string, c: Case): { options: ContextOptions; arm: Arm } {
  writeRepositories(path.join(root, "repos"));
  writeTree(path.join(root, "old"), { "SKILL.md": ALPHA_ARM });
  const arm = buildArm(
    c,
    "old",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  return {
    options: {
      source: localSource(path.join(root, "repos")),
      cacheDir: path.join(root, "cache"),
      root: path.join(root, "scratch"),
    },
    arm,
  };
}

test("the git blob id, the frontmatter description and the role of a context are read as their owners read them", () => {
  // `printf 'hello\n' | git hash-object --stdin`
  expect(gitBlobId(new TextEncoder().encode("hello\n"))).toBe(
    "ce013625030ba8dba906f756967f9e9ca394464a"
  );
  writeTree(path.join(dir, "folded"), { "SKILL.md": ALPHA });
  expect(skillFrontmatter(path.join(dir, "folded"))).toEqual({
    description: "Use when the alpha list is asked for.",
    hidden: false,
  });
  writeTree(path.join(dir, "hidden"), {
    "SKILL.md": "---\nname: h\ndescription: d\ndisable-model-invocation: true\n---\n",
  });
  expect(skillFrontmatter(path.join(dir, "hidden")).hidden).toBe(true);
  writeTree(path.join(dir, "bare"), { "SKILL.md": "# No frontmatter\n" });
  expect(skillFrontmatter(path.join(dir, "bare"))).toEqual({ description: "", hidden: false });
  expect(legionRole("legion:acme:implementer")).toEqual({ role: "implementer", isRoot: false });
  expect(legionRole("legion:acme:architect-root")).toEqual({ role: "architect", isRoot: true });
  expect(legionRole("devbox")).toBeUndefined();
});

// Catches: the tested copy's SKILL.md read with its byte order mark kept, so its frontmatter was
// none (no `---` at its start) while load.ts's read of the session's copy, which drops the mark as
// Oh My Pi's `Bun.file(…).text()` does, found the description: two equal files differed.
test("a SKILL.md that starts with a byte order mark is read as Oh My Pi reads it: the mark dropped, its frontmatter found", () => {
  writeTree(path.join(dir, "bom"), {
    "SKILL.md": "\uFEFF---\nname: bom\ndescription: Use when the file starts with a mark.\n---\n",
  });
  expect(skillFrontmatter(path.join(dir, "bom"))).toEqual({
    description: "Use when the file starts with a mark.",
    hidden: false,
  });
});

// Catches: the commands that build a context (`tar`, `bun install`, `bun run build`, `go test`)
// run with no limit, so one that never exits held the capture for ever.
test("a command that builds a context and never exits is ended at its limit, and fails naming the command and the limit", async () => {
  const root = path.join(dir, "command-hangs");
  mkdirSync(root, { recursive: true });
  const tar = path.join(root, "tar");
  writeFileSync(tar, "#!/bin/sh\nsleep 5\n", { mode: 0o755 });
  const started = performance.now();
  await expect(mustRun([tar, "-xzf", "a.tgz"], 200)).rejects.toThrow(
    `\`${tar} -xzf a.tgz\` ran past its limit of 200 ms, so it was ended`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
});

test("each skill is its repository's directory at its commit, through the cache, and the case's skill is the arm's", async () => {
  const root = path.join(dir, "skills");
  const c = aCase("devbox", {
    contextFiles: [{ repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md", user: true }],
  });
  const { options, arm } = setUp(root, c);
  const first = await buildContext(c, arm, options);
  expect(first.skills.map((skill) => skill.name)).toEqual(["alpha", "beta"]);
  expect(readFileSync(path.join(first.skillsDir, "alpha/SKILL.md"), "utf8")).toBe(ALPHA_ARM);
  expect(readFileSync(path.join(first.skillsDir, "beta/references/r.md"), "utf8")).toBe(
    "Beta reference.\n"
  );
  expect(first.skills[1]?.description).toBe("Use for the beta greeting.");
  expect(first.record.skills[0]).toEqual({ name: "alpha", from: "arm" });
  expect(first.record.builds.map((build) => [build.kind, build.hit])).toEqual([
    ["skill", false],
    ["file", false],
  ]);
  const none = buildArm(
    c,
    "none",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  const again = await buildContext(c, none, options);
  expect(again.skills.map((skill) => skill.name)).toEqual(["beta"]);
  expect(existsSync(path.join(again.skillsDir, "alpha"))).toBe(false);
  expect(again.record.builds.map((build) => [build.kind, build.hit])).toEqual([
    ["skill", true],
    ["file", true],
  ]);
  // A devbox's context file is its user-level one: CLAUDE.md in the scratch Claude Code config
  // directory the host's run names, not a file of the working directory; and it has no role
  // prompt.
  expect(again.claudeConfigDir).toBe(path.join(options.root, "claude-config"));
  expect(readFileSync(path.join(again.claudeConfigDir ?? "", "CLAUDE.md"), "utf8")).toBe(
    USER_RULES
  );
  expect(existsSync(path.join(again.workspace, ".claude"))).toBe(false);
  expect(again.record.contextFiles.map((file) => file.as)).toEqual(["user level"]);
  expect(again.appendSystemPrompt).toBeUndefined();
  expect(again.record.rolePrompt).toBeNull();
});

// Catches: a read of another skill answered from a copy other than the one the capture installed,
// or from one that differs between arms.
test("the installed copy of each repository skill a context installs is its cache entry, the one every arm's context copies, and the arm's own skill is none", async () => {
  const root = path.join(dir, "copies");
  const c = aCase("devbox", {});
  const { options, arm } = setUp(root, c);
  const old = await buildContext(c, arm, options);
  const beta = {
    name: "beta",
    dir: path.join(options.cacheDir, "skills", cacheKey("acme/skills", COMMIT, "skills/beta")),
  };
  expect(installedCopies(old.record, options.cacheDir)).toEqual([beta]);
  expect(filesUnder(beta.dir)).toEqual(filesUnder(path.join(old.skillsDir, "beta")));
  const none = buildArm(
    c,
    "none",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  const absent = await buildContext(c, none, options);
  expect(installedCopies(absent.record, options.cacheDir)).toEqual([beta]);
});

/** A made-up plugin of acme/legion that is an Oh My Pi extension and runs no build: it ships its
 *  extension's source, which Oh My Pi loads as it is. */
const LISTED_PLUGIN = {
  repo: "acme/legion",
  commit: COMMIT,
  path: "packages/plugin-listed",
  name: "plugin-listed",
};

/** A devbox case whose profile installs LISTED_PLUGIN, its context built once under `root`: its
 *  case, arm and options, the options of a second build under a fresh root (`again`), and the
 *  cache entry of the plugin's checkout (`tree`) with that entry's file list (`list`). */
async function listedPluginBuilt(root: string) {
  const c = aCase("devbox", { plugins: [LISTED_PLUGIN], contextFiles: [] });
  const { options, arm } = setUp(root, c);
  writeTree(path.join(root, "repos", "acme", "legion", COMMIT), {
    [`${LISTED_PLUGIN.path}/package.json`]: JSON.stringify({
      name: LISTED_PLUGIN.name,
      files: ["extensions"],
      omp: { extensions: ["extensions/listed.ts"] },
    }),
    [`${LISTED_PLUGIN.path}/extensions/listed.ts`]: "export default function plugin(): void {}\n",
  });
  const first = await buildContext(c, arm, options);
  expect(first.record.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
    [LISTED_PLUGIN.name, "extension"],
  ]);
  const key = cacheKey(LISTED_PLUGIN.repo, COMMIT);
  return {
    c,
    arm,
    again: { ...options, root: path.join(root, "again") },
    tree: path.join(options.cacheDir, "trees", key),
    list: path.join(options.cacheDir, "lists", "trees", `${key}.json`),
  };
}

// Catches: a cached checkout restored from a save that kept only files newer than a cutoff, which
// lacked its plugins' package.json, so the plugins were installed as `skills only`, their tools
// and agents left out, and nothing said so.
test("a cached checkout that lacks a file it was made with is refused, naming the file's full path, where its plugin would have been installed as skills only", async () => {
  const built = await listedPluginBuilt(path.join(dir, "listed-missing"));
  const manifest = path.join(built.tree, LISTED_PLUGIN.path, "package.json");
  rmSync(manifest);
  const failed = await buildContext(built.c, built.arm, built.again).then(
    (context) => context.record.plugins,
    (error: unknown) => error
  );
  expect(failed).toBeInstanceOf(IncompleteCacheEntry);
  expect(String(failed)).toContain(`${manifest} (missing)`);
});

// Catches: a cached checkout made before file lists were kept taken as it is, though a save cut
// by file time may have kept only part of it: only a tree keeps its source's file times.
test("a cached checkout with no file list is made again, with its list, and its plugin is installed", async () => {
  const built = await listedPluginBuilt(path.join(dir, "listed-unlisted"));
  rmSync(built.list, { force: true });
  const again = await buildContext(built.c, built.arm, built.again);
  expect(again.record.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
    [LISTED_PLUGIN.name, "extension"],
  ]);
  expect(again.record.builds.filter((build) => build.kind === "tree")).toMatchObject([
    { hit: false },
  ]);
  expect(existsSync(built.list)).toBe(true);
});

// Catches: a cached skill whose file changed since it was made copied into the context as it is.
test("a cached skill whose file's size differs from its list's is refused, naming the file and both sizes", async () => {
  const root = path.join(dir, "listed-skill");
  const c = aCase("devbox", { contextFiles: [] });
  const { options, arm } = setUp(root, c);
  await buildContext(c, arm, options);
  const file = path.join(
    options.cacheDir,
    "skills",
    cacheKey("acme/skills", COMMIT, "skills/beta"),
    "references/r.md"
  );
  writeFileSync(file, "Beta.\n");
  const failed = await buildContext(c, arm, { ...options, root: path.join(root, "again") }).then(
    (context) => context.record.skills,
    (error: unknown) => error
  );
  expect(failed).toBeInstanceOf(IncompleteCacheEntry);
  expect(String(failed)).toContain(`${file} (size 16 recorded, 6 found)`);
});

// Catches: a file the entry did not hold when it was made (a compose writes its Go test into a
// checkout while it runs) taken for a change to the entry.
test("a file written into a cached checkout after it was made is no change to its list, and the checkout is a hit", async () => {
  const built = await listedPluginBuilt(path.join(dir, "listed-added"));
  expect(existsSync(built.list)).toBe(true);
  writeTree(built.tree, { [`${LISTED_PLUGIN.path}/added/note.md`]: "Written after the build.\n" });
  const again = await buildContext(built.c, built.arm, built.again);
  expect(again.record.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
    [LISTED_PLUGIN.name, "extension"],
  ]);
  expect(again.record.builds.filter((build) => build.kind === "tree")).toMatchObject([
    { hit: true },
  ]);
});

test("a second copy of a name whose text differs goes in a skills directory of its namespace, which the capture gives Oh My Pi after skills/, so it lists the copy as <namespace>/<name>", async () => {
  const root = path.join(dir, "namespaced");
  const c = aCase("devbox", {});
  c.profile.skills.push({
    repo: "acme/tools",
    commit: COMMIT,
    path: "skills/beta",
    name: "beta",
    namespace: "tools",
  });
  const { options, arm } = setUp(root, c);
  const tools =
    "---\nname: beta\ndescription: Use for the tools' own beta.\n---\n\n# beta of tools\n";
  writeTree(path.join(root, "repos", "acme", "tools", COMMIT, "skills", "beta"), {
    "SKILL.md": tools,
  });
  const built = await buildContext(c, arm, options);
  const namespaced = path.join(options.root, "namespaced", "tools", "skills");
  expect(built.namespacedDirs).toEqual([namespaced]);
  expect(readFileSync(path.join(namespaced, "beta", "SKILL.md"), "utf8")).toBe(tools);
  // The bare beta stays acme/skills' copy, in skills/.
  expect(readFileSync(path.join(built.skillsDir, "beta", "SKILL.md"), "utf8")).not.toBe(tools);
  expect(built.skills.map((skill) => skill.name)).toEqual(["alpha", "beta", "tools/beta"]);
  expect(built.skills[2]?.description).toBe("Use for the tools' own beta.");
});

// Catches: a copy installed under a name its SKILL.md's frontmatter does not give it. Oh My Pi
// names a skill by its frontmatter `name`, so it listed the copy, asked its hint and found its
// collisions under another name than the profile's, and the capture failed only after its hint
// run had waited for a hint Oh My Pi never asked for.
test("an installed copy whose frontmatter names another skill is refused, naming the case, its source and both names; so is an arm text that renames the case's skill", async () => {
  const root = path.join(dir, "misnamed");
  const c = aCase("devbox", {});
  c.profile.skills.push({
    repo: "acme/tools",
    commit: COMMIT,
    path: "skills/gamma",
    name: "gamma",
  });
  const { options, arm } = setUp(root, c);
  writeTree(path.join(root, "repos", "acme", "tools", COMMIT, "skills", "gamma"), {
    "SKILL.md": "---\nname: delta\ndescription: Use for the made-up delta.\n---\n\n# Delta\n",
  });
  const refused = await buildContext(c, arm, options).catch((caught: unknown) => caught);
  expect(refused).toBeInstanceOf(Refused);
  expect((refused as Error).message).toBe(
    `case case-devbox: the profile's skill gamma is installed from skills/gamma of acme/tools at ${COMMIT}, whose SKILL.md's frontmatter names it delta, and Oh My Pi names a skill by its frontmatter name, not gamma, the name the profile gives it`
  );

  const renamed = aCase("devbox", {});
  const other = path.join(root, "renamed");
  writeTree(other, { "SKILL.md": ALPHA_ARM.replace(/^name: alpha$/m, "name: alpha-two") });
  const renamedArm = buildArm(renamed, "new", { old: path.join(root, "old"), new: other }, root);
  const refusedArm = await buildContext(renamed, renamedArm, {
    ...options,
    root: path.join(root, "scratch-renamed"),
  }).catch((caught: unknown) => caught);
  expect(refusedArm).toBeInstanceOf(Refused);
  expect((refusedArm as Error).message).toBe(
    `case case-devbox: the profile's skill alpha is installed from the arm's text, ${other}, whose SKILL.md's frontmatter names it alpha-two, and Oh My Pi names a skill by its frontmatter name, not alpha, the name the profile gives it: an arm's text must keep the case skill's name`
  );
});

// Catches: a none arm that lists another source's copy under the case skill's name taken for the
// arm's own copy (cases.ts's testedCopy finds the copy listed under the name), so its context
// refused it as an arm with no skill; or that copy left in its namespace's directory.
test("each copy of the case's skill's source, a namespaced one at another commit too, is the arm's directory, and a copy of another source that shares the name is its repository's in every arm; none installs neither copy of the source, and installs the other source's copy from its repository under the bare name it lists it under", async () => {
  const root = path.join(dir, "tested-copies");
  const c = aCase("devbox", {});
  c.profile.skills.push(
    // acme/skills' alpha at a commit the made-up repositories do not hold: never read.
    {
      repo: "acme/skills",
      commit: "f".repeat(40),
      path: "skills/alpha",
      name: "alpha",
      namespace: "pack",
    },
    { repo: "acme/tools", commit: COMMIT, path: "skills/alpha", name: "alpha", namespace: "tools" }
  );
  const { options, arm } = setUp(root, c);
  const tools =
    "---\nname: alpha\ndescription: Use for the tools' own alpha.\n---\n\n# alpha of tools\n";
  writeTree(path.join(root, "repos", "acme", "tools", COMMIT, "skills", "alpha"), {
    "SKILL.md": tools,
  });
  const built = await buildContext(c, arm, options);
  const namespaced = (ns: string) =>
    path.join(options.root, "namespaced", ns, "skills", "alpha", "SKILL.md");
  expect(readFileSync(path.join(built.skillsDir, "alpha", "SKILL.md"), "utf8")).toBe(ALPHA_ARM);
  expect(readFileSync(namespaced("pack"), "utf8")).toBe(ALPHA_ARM);
  expect(readFileSync(namespaced("tools"), "utf8")).toBe(tools);
  expect(
    built.record.skills.filter((skill) => skill.from === "arm").map((skill) => skill.name)
  ).toEqual(["alpha", "pack/alpha"]);
  const none = buildArm(
    c,
    "none",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  const without = await buildContext(c, none, options);
  expect(without.skills.map((skill) => skill.name)).toEqual(["beta", "alpha"]);
  expect(readFileSync(path.join(without.skillsDir, "alpha", "SKILL.md"), "utf8")).toBe(tools);
  expect(without.namespacedDirs).toEqual([]);
  expect(existsSync(namespaced("tools"))).toBe(false);
  expect(without.record.skills.find((skill) => skill.name === "alpha")?.from).toEqual({
    repo: "acme/tools",
    commit: COMMIT,
    path: "skills/alpha",
    key: cacheKey("acme/tools", COMMIT, "skills/alpha"),
  });
  expect(none.relisted).toEqual(["tools/alpha"]);
});

test("a devbox profile's user-level .claude/CLAUDE.md (user: true) goes in the Claude Code config directory, and the root CLAUDE.md or AGENTS.md of its session's repository in the working directory; two user-level files are refused", async () => {
  const root = path.join(dir, "devbox-files");
  const c = aCase("devbox", {
    contextFiles: [
      { repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/project", commit: COMMIT, path: "CLAUDE.md" },
    ],
  });
  const { options, arm } = setUp(root, c);
  const built = await buildContext(c, arm, options);
  expect(built.claudeConfigDir).toBe(path.join(options.root, "claude-config"));
  expect(readFileSync(path.join(built.claudeConfigDir ?? "", "CLAUDE.md"), "utf8")).toBe(
    USER_RULES
  );
  // Oh My Pi reads the repository's own file from the working directory, as for a Legion pane.
  expect(readFileSync(path.join(built.workspace, "CLAUDE.md"), "utf8")).toBe(PROJECT_RULES);
  expect(existsSync(path.join(built.workspace, ".claude"))).toBe(false);
  expect(built.record.contextFiles.map((file) => file.as)).toEqual([
    "user level",
    "working directory",
  ]);
  expect(built.appendSystemPrompt).toBeUndefined();
  // A devbox profile with no user-level file has no Claude Code config directory.
  const projectOnly = aCase("devbox", {
    contextFiles: [{ repo: "acme/project", commit: COMMIT, path: "AGENTS.md" }],
  });
  const alone = await buildContext(projectOnly, arm, options);
  expect(alone.claudeConfigDir).toBeUndefined();
  expect(existsSync(path.join(options.root, "claude-config"))).toBe(false);
  expect(readFileSync(path.join(alone.workspace, "AGENTS.md"), "utf8")).toBe(AGENTS);
  expect(alone.record.contextFiles.map((file) => file.as)).toEqual(["working directory"]);
  // Oh My Pi reads one user-level context file, so a profile of two is refused.
  const two = aCase("devbox", {
    contextFiles: [
      { repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/infra", commit: COMMIT, path: ".claude/CLAUDE.md", user: true },
    ],
  });
  expect(await buildContext(two, arm, options).catch(String)).toContain(
    "its profile holds 2 user-level context files, and Oh My Pi reads one"
  );
});

test("the session repository's context files are written at their paths in its checkout and the working directory is the profile's workingDirectory there, so each is at the depth the session read it; its own .claude/CLAUDE.md is no user-level file", async () => {
  const root = path.join(dir, "levels");
  const c = aCase("devbox", {
    contextFiles: [
      { repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md", user: true },
      { repo: "acme/project", commit: COMMIT, path: "CLAUDE.md" },
      { repo: "acme/project", commit: COMMIT, path: "agents/vm/AGENTS.md" },
      { repo: "acme/project", commit: COMMIT, path: "agents/vm/.claude/CLAUDE.md" },
      { repo: "acme/project", commit: COMMIT, path: "agents/vm/.github/copilot-instructions.md" },
    ],
    workingDirectory: "agents/vm",
  });
  const { options, arm } = setUp(root, c);
  const built = await buildContext(c, arm, options);
  const checkout = path.join(options.root, "work");
  expect(built.checkout).toBe(checkout);
  expect(built.workspace).toBe(path.join(checkout, "agents", "vm"));
  expect(readFileSync(path.join(checkout, "CLAUDE.md"), "utf8")).toBe(PROJECT_RULES);
  expect(readFileSync(path.join(built.workspace, "AGENTS.md"), "utf8")).toBe(VM_AGENTS);
  expect(readFileSync(path.join(built.workspace, ".claude", "CLAUDE.md"), "utf8")).toBe(VM_CLAUDE);
  expect(
    readFileSync(path.join(built.workspace, ".github", "copilot-instructions.md"), "utf8")
  ).toBe(VM_COPILOT);
  // Only the file marked user: true is the user-level one.
  expect(readFileSync(path.join(built.claudeConfigDir ?? "", "CLAUDE.md"), "utf8")).toBe(
    USER_RULES
  );
  expect(existsSync(path.join(checkout, ".claude"))).toBe(false);
  expect(built.record.contextFiles.map((file) => file.as)).toEqual([
    "user level",
    "working directory",
    "working directory",
    "working directory",
    "working directory",
  ]);
  expect(built.appendSystemPrompt).toBeUndefined();
});

describe.skipIf(goMissing && !onActions)("a Legion context", () => {
  test("its system prompt file is the role prompt Compose names at the plugin repository's commit, then the instructions file, and its plugins are built or left out", async () => {
    const root = path.join(dir, "legion");
    const c = aCase("legion:acme:implementer", {
      plugins: [
        { repo: "acme/legion", commit: COMMIT, path: "packages/plugin-a", name: "plugin-a" },
        { repo: "acme/legion", commit: COMMIT, path: "packages/plugin-b", name: "plugin-b" },
        { repo: "acme/legion", commit: COMMIT, path: "plugins/skills-only", name: "skills-only" },
      ],
      contextFiles: [
        { repo: "acme/infra", commit: COMMIT, path: "legion/instructions/acme.md" },
        { repo: "acme/project", commit: COMMIT, path: "AGENTS.md" },
      ],
    });
    const { options, arm } = setUp(root, c);
    const built = await buildContext(c, arm, options);
    expect(readFileSync(built.appendSystemPrompt ?? "", "utf8")).toBe(
      "Core part.\nImplementer part.\nWorker part.\n\n# Acme instructions\n\nWork the acme way."
    );
    expect(built.record.rolePrompt).toMatchObject({
      repo: "acme/legion",
      commit: COMMIT,
      role: "implementer",
      isRoot: false,
      promptsBlob: gitBlobId(new TextEncoder().encode(DAEMON[PROMPTS_GO])),
      parts: [
        "packages/daemon/internal/prompts/roles/core/common.md",
        "packages/daemon/internal/prompts/roles/implementer.md",
        "packages/daemon/internal/prompts/go/worker.md",
      ],
    });
    expect(built.record.contextFiles.map((file) => file.as)).toEqual([
      "system prompt",
      "working directory",
    ]);
    expect(built.record.leftOut.join(" ")).toContain("addressing sentence");
    // Compose ran once, in the cached copy of the repository, and its Go test is gone from it.
    expect(built.record.builds.filter((build) => build.kind === "compose")).toMatchObject([
      { hit: false },
    ]);
    const internal = path.join(
      options.cacheDir,
      "trees",
      cacheKey("acme/legion", COMMIT),
      "packages/daemon/internal"
    );
    expect(readdirSync(internal).sort()).toEqual(["claim", "prompts"]);

    // plugin-a is built, ships its bundle, and its skills path is the profile's skills.
    const plugins = Object.fromEntries(built.record.plugins.map((p) => [p.name, p]));
    expect(plugins["plugin-a"]?.as).toBe("extension");
    expect(plugins["plugin-b"]?.as).toBe("left out");
    expect(plugins["plugin-b"]?.reason).toContain("exit 3");
    expect(plugins["skills-only"]?.as).toBe("skills only");
    const pluginA = path.join(options.root, "plugins/plugin-a");
    expect(built.extensions).toEqual([pluginA]);
    const manifest = JSON.parse(readFileSync(path.join(pluginA, "package.json"), "utf8"));
    expect(manifest.omp.extensions).toEqual(["dist/a.js"]);
    expect(existsSync(path.join(pluginA, "dist/a.js"))).toBe(true);
    expect(lstatSync(path.join(pluginA, "dist/skills")).isSymbolicLink()).toBe(true);
    expect(realpathSync(path.join(pluginA, "dist/skills"))).toBe(realpathSync(built.skillsDir));

    // Built again, every build is the cache's.
    const again = await buildContext(c, arm, options);
    expect(readFileSync(again.appendSystemPrompt ?? "", "utf8")).toBe(
      readFileSync(built.appendSystemPrompt ?? "", "utf8")
    );
    const kinds = again.record.builds.filter((build) => build.kind !== "tree");
    expect(kinds.every((build) => build.hit)).toBe(true);
    expect(kinds.some((build) => build.kind === "compose")).toBe(true);
  }, 300_000);

  test("a controller's role prompt is the parts of a headless controller, and the instructions file follows it", async () => {
    const root = path.join(dir, "controller");
    const c = aCase("legion:acme:controller", {
      plugins: [
        { repo: "acme/legion", commit: COMMIT, path: "plugins/skills-only", name: "skills-only" },
      ],
      contextFiles: [{ repo: "acme/infra", commit: COMMIT, path: "legion/instructions/acme.md" }],
    });
    const { options, arm } = setUp(root, c);
    const built = await buildContext(c, arm, options);
    expect(readFileSync(built.appendSystemPrompt ?? "", "utf8")).toBe(
      "Controller part.\nHeadless controller part.\n\n# Acme instructions\n\nWork the acme way."
    );
    expect(built.record.rolePrompt).toMatchObject({
      role: "controller",
      isRoot: false,
      parts: [
        "packages/daemon/internal/prompts/roles/controller-root.md",
        "packages/daemon/internal/prompts/go/controller-headless.md",
      ],
    });
  }, 300_000);

  // Catches: the role prompt composed at the plugin's commit (the worker image's) where the
  // project's daemon image, which composes it, was built from another commit.
  test("the role prompt is Compose's at the commit the case's evidence names as its daemon image's, and at the plugin's commit where the evidence names none, the record saying which", async () => {
    const root = path.join(dir, "daemon-commit");
    /** The made-up commit of acme/legion that the project's daemon image was built from. */
    const DAEMON_COMMIT = "fedcba9876543210fedcba9876543210fedcba98";
    const c = aCase("legion:acme:implementer", {
      plugins: [
        { repo: "acme/legion", commit: COMMIT, path: "plugins/skills-only", name: "skills-only" },
      ],
      contextFiles: [],
    });
    const { options, arm } = setUp(root, c);
    // acme/legion at the daemon image's commit: the same composer, another implementer part.
    writeTree(path.join(root, "repos", "acme/legion", DAEMON_COMMIT), {
      ...DAEMON,
      "packages/daemon/internal/prompts/roles/implementer.md":
        "Implementer part at the daemon image's commit.\n",
    });
    const item = "the daemon image's commit of acme/legion";
    const project = `acme/infra:legion/projects/10-ACME.yaml at ${COMMIT}`;
    const digest = "8".repeat(64);
    const named: Case = {
      ...c,
      evidence: [
        {
          item,
          source: `${project} gives the project ACME the daemonImage registry.example/acme/daemon@sha256:${digest}; gh api repos/acme/legion/attestations/sha256:${digest} names git+https://github.com/acme/legion@refs/heads/main at ${DAEMON_COMMIT}, so the role prompt is composed from acme/legion at ${DAEMON_COMMIT}: the Legion daemon of a pod composes its role prompt as it was at the commit its daemon image was built from`,
        },
      ],
    };
    const atDaemon = await buildContext(named, arm, options);
    expect(readFileSync(atDaemon.appendSystemPrompt ?? "", "utf8")).toBe(
      "Core part.\nImplementer part at the daemon image's commit.\nWorker part."
    );
    expect(atDaemon.record.rolePrompt).toMatchObject({
      repo: "acme/legion",
      commit: DAEMON_COMMIT,
      commitOf: "daemon image",
      role: "implementer",
    });

    // An item that names no commit, as build.ts writes one where no rule gave it, and no item.
    const unnamed: Case = {
      ...c,
      evidence: [
        {
          item,
          source: `${project} has no top-level daemonImage: line, and acme/infra:legion/Pulumi.prod.yaml at ${COMMIT} does not exist, so the role prompt is composed at the commit the profile reads the plugins of acme/legion at: the Legion daemon of a pod composes its role prompt as it was at the commit its daemon image was built from`,
        },
      ],
    };
    for (const each of [unnamed, c]) {
      const built = await buildContext(each, arm, options);
      expect(readFileSync(built.appendSystemPrompt ?? "", "utf8")).toBe(
        "Core part.\nImplementer part.\nWorker part."
      );
      expect(built.record.rolePrompt).toMatchObject({
        repo: "acme/legion",
        commit: COMMIT,
        commitOf: "plugin",
      });
    }
  }, 60_000);
});

// A SKILL.md's frontmatter as Oh My Pi 18.18.6 reads it (packages/utils/src/frontmatter.ts at the
// pin): a block plain YAML refuses is repaired, or read line by line, never a throw.
test("a description holding ': ' in a plain scalar is read whole, as Oh My Pi repairs it", () => {
  const text =
    "---\nname: lookup\ndescription: Use to look things up. Examples: current events, prices\n---\n\n# Lookup\n";
  expect(() =>
    Bun.YAML.parse("description: Use to look things up. Examples: current events")
  ).toThrow();
  expect(readFrontmatter(text)).toEqual({
    name: "lookup",
    description: "Use to look things up. Examples: current events, prices",
  });
  writeTree(path.join(dir, "colon"), { "SKILL.md": text });
  expect(skillFrontmatter(path.join(dir, "colon"))).toEqual({
    description: "Use to look things up. Examples: current events, prices",
    hidden: false,
  });
});

test("a tab in the frontmatter is read as two spaces, and kebab-case keys as camelCase", () => {
  const text =
    "---\r\nname: tabbed\r\nmetadata:\r\n\tkind: made-up\r\ndescription: Tabbed.\r\ndisable-model-invocation: true\r\n---\r\n";
  expect(readFrontmatter(text)).toEqual({
    name: "tabbed",
    metadata: { kind: "made-up" },
    description: "Tabbed.",
    disableModelInvocation: true,
  });
});

test("a block no repair parses is read one key: value line at a time, a value YAML refuses kept as its text", () => {
  const text =
    "<!-- a made-up comment -->\n---\nname: broken\ndescription: [unclosed: list\nhide-me: x\n  nested: [also\ndisable-model-invocation: true\n---\nBody.\n";
  expect(readFrontmatter(text)).toEqual({
    name: "broken",
    description: "[unclosed: list",
    hideMe: "x",
    disableModelInvocation: true,
  });
  writeTree(path.join(dir, "broken"), { "SKILL.md": text });
  expect(skillFrontmatter(path.join(dir, "broken"))).toEqual({
    description: "[unclosed: list",
    hidden: true,
  });
  expect(readFrontmatter("# No frontmatter\n")).toEqual({});
});

test("comments inside the block are dropped in one left-to-right pass, as Oh My Pi drops them", () => {
  const text =
    "---\nname: x<!-- gone -->y\ndescription: keep <!<!---->-- this\nnote: open <!-- no end\n---\nBody.\n";
  expect(readFrontmatter(text)).toEqual({
    name: "xy",
    description: "keep <!-- this",
    note: "open <!-- no end",
  });
});

// Catches: nested keys left in kebab-case, where Oh My Pi's `normalizeFrontmatterKeys` makes each
// key camelCase at every depth: two copies of a skill that differ only so read as different,
// though Oh My Pi drops one as the other (capture.ts's `listedSkills`).
test("kebab-case keys become camelCase at every depth, in a list's mappings too, and no value changes", () => {
  const text =
    "---\nname: nested\nmetadata:\n  made-up-kind: greeting\n  steps:\n    - step-name: one\n      keep: [a-b]\n---\nBody.\n";
  expect(readFrontmatter(text)).toEqual({
    name: "nested",
    metadata: { madeUpKind: "greeting", steps: [{ stepName: "one", keep: ["a-b"] }] },
  });
});

/** A stand-in `gh` that answers its first `fails` calls with a network timeout on stderr, then
 *  with `answer`; each call is counted in the file `count`. */
function standinGh(name: string, fails: number, answer: string): { gh: string; count: string } {
  const root = path.join(dir, name);
  const count = path.join(root, "count");
  const gh = path.join(root, "gh");
  writeTree(root, {
    gh: `#!/bin/sh
n=$(cat ${JSON.stringify(count)} 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > ${JSON.stringify(count)}
if [ "$n" -le ${fails} ]; then
  echo 'Get "https://api.example.test/repos/acme/project/contents/AGENTS.md": dial tcp: lookup api.example.test: i/o timeout' >&2
  exit 1
fi
printf '%s' ${JSON.stringify(answer)}
`,
  });
  chmodSync(gh, 0o755);
  return { gh, count };
}

test("a gh read that times out twice is sent again after 5 s and 15 s, as copies.ts's reads are, and answers; the source counts both retries", async () => {
  const { gh, count } = standinGh("gh-twice", 2, "# Project");
  const waits: number[] = [];
  const said: string[] = [];
  const source = ghSource(gh, {
    sleep: async (ms) => void waits.push(ms),
    err: (line) => void said.push(line),
  });
  const bytes = await source.file("acme/project", COMMIT, "AGENTS.md");
  expect(new TextDecoder().decode(bytes)).toBe("# Project");
  expect(readFileSync(count, "utf8").trim()).toBe("3");
  expect(waits).toEqual([5_000, 15_000]);
  expect(said).toHaveLength(2);
  expect(said[1]).toEndWith("sending it again in 15 s, retry 2 of 4");
  expect(source.reads?.()).toMatchObject({ calls: 3, retries: { "i/o timeout": 2 } });
}, 120_000);

test("a gh read that always fails stops after 4 tries more, naming the command and each try's error", async () => {
  const { gh, count } = standinGh("gh-always", 99, "");
  const waits: number[] = [];
  const source = ghSource(gh, { sleep: async (ms) => void waits.push(ms), err: () => {} });
  const failed = await source
    .file("acme/project", COMMIT, "AGENTS.md")
    .then(() => "answered")
    .catch((error: unknown) => String(error));
  expect(failed).toContain(
    `\`${gh} api --method GET -H Accept: application/vnd.github.raw repos/acme/project/contents/AGENTS.md?ref=${COMMIT}\` failed: 5 tries: `
  );
  for (const n of [1, 2, 3, 4, 5]) expect(failed).toContain(`try ${n}: Get "https://`);
  expect(failed.match(/i\/o timeout/g)?.length).toBe(5);
  expect(readFileSync(count, "utf8").trim()).toBe("5");
  expect(waits).toEqual([5_000, 15_000, 45_000, 135_000]);
}, 120_000);

test("a gh read that fails for good, such as a 404, is NoSuchFile at once: one call, no wait", async () => {
  const root = path.join(dir, "gh-missing");
  const count = path.join(root, "count");
  writeTree(root, {
    gh: `#!/bin/sh\necho x >> ${JSON.stringify(count)}\necho 'gh: Not Found (HTTP 404)' >&2\nexit 1\n`,
  });
  chmodSync(path.join(root, "gh"), 0o755);
  const waits: number[] = [];
  const said: string[] = [];
  const source = ghSource(path.join(root, "gh"), {
    sleep: async (ms) => void waits.push(ms),
    err: (line) => void said.push(line),
  });
  const failed = await source
    .file("acme/project", COMMIT, "missing.md")
    .catch((error: unknown) => error);
  // A 404 is a file the commit does not hold, told apart from a read that failed.
  expect(failed).toBeInstanceOf(NoSuchFile);
  expect(String(failed)).toContain("failed: gh: Not Found (HTTP 404)");
  expect(readFileSync(count, "utf8")).toBe("x\n");
  expect(waits).toEqual([]);
  expect(said).toEqual([]);
  expect(source.reads?.().calls).toBe(1);
  expect(source.reads?.().retries).toEqual({});
}, 120_000);

// Catches: a read refused for the primary rate limit waited out to the reset `gh api rate_limit`
// gives, which names no owner, so the pod's gh answers it from another installation than the one
// that refused the read: where that reset comes sooner, each try is refused again.
test("a gh read refused for the primary rate limit waits until the reset of its repository's installation, read with gh api -i, not the sooner one gh api rate_limit gives, and answers", async () => {
  const root = path.join(dir, "gh-rate-limited");
  const log = path.join(root, "log");
  const count = path.join(root, "count");
  const reset = Math.floor(Date.now() / 1000) + 600;
  const sooner = Math.floor(Date.now() / 1000) + 60;
  writeTree(root, {
    gh: `#!/bin/sh
echo "$*" >> ${JSON.stringify(log)}
case "$*" in
  "api -i --method GET repos/acme/project")
    printf 'HTTP/2.0 200 OK\\r\\nX-Ratelimit-Reset: ${reset}\\r\\n\\r\\n{}'
    exit 0 ;;
  *rate_limit*)
    printf '{"resources":{"core":{"reset":${sooner}},"graphql":{"reset":${sooner}}}}'
    exit 0 ;;
esac
n=$(cat ${JSON.stringify(count)} 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > ${JSON.stringify(count)}
if [ "$n" -le 1 ]; then
  echo 'gh: API rate limit exceeded for installation ID 1. (HTTP 403)' >&2
  exit 1
fi
printf '%s' '# Project'
`,
  });
  chmodSync(path.join(root, "gh"), 0o755);
  const waits: number[] = [];
  const said: string[] = [];
  const source = ghSource(path.join(root, "gh"), {
    sleep: async (ms) => void waits.push(ms),
    err: (line) => void said.push(line),
  });
  const before = Date.now();
  const bytes = await source.file("acme/project", COMMIT, "AGENTS.md");
  const after = Date.now();
  expect(new TextDecoder().decode(bytes)).toBe("# Project");
  expect(readFileSync(count, "utf8").trim()).toBe("2");
  // The one read of a reset is a GET of the repository, which the pod's gh sends with its owner's
  // installation.
  expect(
    readFileSync(log, "utf8")
      .split("\n")
      .filter((line) => line !== "" && !line.includes("contents/AGENTS.md"))
  ).toEqual(["api -i --method GET repos/acme/project"]);
  expect(waits).toHaveLength(1);
  expect(waits[0]).toBeGreaterThanOrEqual(reset * 1000 + 1_000 - after);
  expect(waits[0]).toBeLessThanOrEqual(reset * 1000 + 1_000 - before);
  expect(
    said.filter((line) =>
      /^context: acme's REST rate limit resets at \S+; waiting \d+ s, until \S+$/.test(line)
    )
  ).toHaveLength(1);
}, 60_000);

// Catches: a read of prompts.go that failed taken as a repository that holds none.
test("a Legion context's role prompt: a plugin repository that holds no prompts.go is passed over, and a read of it that failed stops the context, naming why", async () => {
  const root = path.join(dir, "prompts-read");
  const c = aCase("legion:acme:implementer", {
    plugins: [
      { repo: "acme/legion", commit: COMMIT, path: "plugins/skills-only", name: "skills-only" },
    ],
    contextFiles: [],
  });
  const { options, arm } = setUp(root, c);
  const local = options.source;
  /** The local source, but its read of prompts.go throws `error`. */
  const failing = (error: Error): RepoSource => ({
    ...local,
    name: "a source whose read of prompts.go fails",
    file: (repo, commit, file) =>
      file === PROMPTS_GO ? Promise.reject(error) : local.file(repo, commit, file),
  });
  const failed = await buildContext(c, arm, {
    ...options,
    source: failing(new Error("gh: connection reset by peer")),
  }).catch(String);
  expect(failed).toContain("gh: connection reset by peer");
  expect(failed).not.toContain("no plugin repository holds");
  const absent = await buildContext(c, arm, {
    ...options,
    source: failing(new NoSuchFile("acme/legion holds no prompts.go")),
  }).catch(String);
  expect(absent).toContain(`no plugin repository holds ${PROMPTS_GO}`);
});

// Catches: a plugin whose repository read failed left out of one arm's context while the next
// arm, reading it again, installs it; and a left-out plugin whose reason does not say which step
// failed.
test("a plugin whose repository read fails stops the context, naming why; a plugin that cannot be installed is left out, its reason naming the step that failed", async () => {
  const root = path.join(dir, "plugin-reads");
  const c = aCase("devbox", {
    plugins: [
      { repo: "acme/legion", commit: COMMIT, path: "packages/plugin-b", name: "plugin-b" },
      { repo: "acme/legion", commit: COMMIT, path: "plugins/broken", name: "broken" },
    ],
    contextFiles: [],
  });
  const { options, arm } = setUp(root, c);
  writeTree(path.join(root, "repos", "acme/legion", COMMIT), {
    "plugins/broken/package.json": "{ not JSON",
  });
  const local = options.source;
  const unread: RepoSource = {
    ...local,
    name: "a source whose read of acme/legion fails",
    tree: (repo, commit, inside, dest) =>
      repo === "acme/legion"
        ? Promise.reject(new Error("gh: connection reset by peer"))
        : local.tree(repo, commit, inside, dest),
  };
  const failed = await buildContext(c, arm, { ...options, source: unread }).then(
    () => "built",
    String
  );
  expect(failed).toContain("plugin-b");
  expect(failed).toContain("gh: connection reset by peer");
  const built = await buildContext(c, arm, options);
  const reasons = Object.fromEntries(built.record.plugins.map((p) => [p.name, p.reason]));
  expect(built.record.plugins.map((p) => p.as)).toEqual(["left out", "left out"]);
  expect(reasons["plugin-b"]).toStartWith("its build failed: ");
  expect(reasons["plugin-b"]).toContain("exit 3");
  expect(reasons.broken).toStartWith("its package.json does not parse: ");
}, 120_000);

/** A file of the stand-in repository: its path, its bytes, whether a GraphQL batch calls it
 *  binary, and its mode (git's: 100644 when absent, 120000 a link whose target is its bytes). */
interface StandinFile {
  readonly path: string;
  readonly bytes: Uint8Array;
  readonly binary?: boolean;
  readonly mode?: "100644" | "120000";
}

/**
 * A stand-in `gh` holding one made-up repository at one commit, a Bun script that logs each call's
 * arguments as one JSON line to its `log` and answers as GitHub does: the recursive tree; a
 * GraphQL batch of blobs by oid (a binary file as `isBinary`, any other as its text decoded as
 * UTF-8, as GitHub decodes it, so bytes that are no UTF-8 come back changed); and a blob alone.
 */
function standinRepoGh(name: string, files: readonly StandinFile[]): { gh: string; log: string } {
  const root = path.join(dir, name);
  const log = path.join(root, "calls.jsonl");
  const gh = path.join(root, "gh");
  const listed = files.map((file) => ({
    path: file.path,
    mode: file.mode ?? "100644",
    sha: gitBlobId(file.bytes),
    size: file.bytes.length,
    b64: Buffer.from(file.bytes).toString("base64"),
    binary: file.binary === true,
  }));
  writeTree(root, {
    gh: `#!${process.execPath}
import { appendFileSync } from "node:fs";
const files = ${JSON.stringify(listed)};
const args = process.argv.slice(2);
appendFileSync(${JSON.stringify(log)}, JSON.stringify(args) + "\\n");
const route = args.at(-1) ?? "";
if (args[1] === "graphql") {
  const query = (args.find((arg) => arg.startsWith("query=")) ?? "").slice(6);
  const repository = {};
  for (const match of query.matchAll(/b(\\d+): object\\(oid: "([0-9a-f]{40})"\\)/g)) {
    const file = files.find((each) => each.sha === match[2]);
    repository["b" + match[1]] = file === undefined ? null : file.binary
      ? { isBinary: true, isTruncated: false, text: null }
      : { isBinary: false, isTruncated: false, text: Buffer.from(file.b64, "base64").toString("utf8") };
  }
  process.stdout.write(JSON.stringify({ data: { repository } }));
} else if (route.includes("/git/trees/")) {
  process.stdout.write(JSON.stringify({
    tree: files.map((file) => ({ path: file.path, mode: file.mode, type: "blob", sha: file.sha, size: file.size })),
    truncated: false,
  }));
} else if (/\\/git\\/blobs\\/[0-9a-f]{40}$/.test(route)) {
  const file = files.find((each) => route.endsWith(each.sha));
  process.stdout.write(Buffer.from(file.b64, "base64"));
} else {
  process.stderr.write("gh: Not Found (HTTP 404)\\n");
  process.exit(1);
}
`,
  });
  chmodSync(gh, 0o755);
  return { gh, log };
}

/** The stand-in's calls, by kind. */
function callsOf(log: string): { tree: number; graphql: number; blob: number; other: number } {
  const calls = readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as string[]);
  const count = (test: (args: string[]) => boolean) => calls.filter(test).length;
  const tree = count((args) => (args.at(-1) ?? "").includes("/git/trees/"));
  const graphql = count((args) => args[1] === "graphql");
  const blob = count((args) => (args.at(-1) ?? "").includes("/git/blobs/"));
  return { tree, graphql, blob, other: calls.length - tree - graphql - blob };
}

/** A case of alpha whose profile also installs each of `dirs` of acme/batch at COMMIT, and its old
 *  arm, under `root`. */
function batchCase(root: string, dirs: readonly string[]): { c: Case; arm: Arm } {
  const c = aCase("devbox", {
    skills: [
      { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" },
      ...dirs.map((each) => ({
        repo: "acme/batch",
        commit: COMMIT,
        path: each,
        name: path.basename(each),
      })),
    ],
    contextFiles: [],
  });
  writeTree(path.join(root, "old"), { "SKILL.md": ALPHA_ARM });
  const arm = buildArm(
    c,
    "old",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  return { c, arm };
}

test("a profile of N files at one commit is read with its tree and ceil(N/50) GraphQL batches, byte for byte", async () => {
  const dirs = ["skills/s1", "skills/s2", "skills/s3"];
  const files: StandinFile[] = dirs.flatMap((each) => [
    {
      path: `${each}/SKILL.md`,
      bytes: new TextEncoder().encode(
        `---\nname: ${path.basename(each)}\ndescription: Made-up ${each}.\n---\n`
      ),
    },
    ...Array.from({ length: 39 }, (_, index) => ({
      path: `${each}/references/r${index}.md`,
      bytes: new TextEncoder().encode(`Reference ${index} of ${each}, é ✓.\n`),
    })),
  ]);
  const { gh, log } = standinRepoGh("gh-batch", files);
  const root = path.join(dir, "batch");
  const { c, arm } = batchCase(root, dirs);
  const options = {
    source: ghSource(gh),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  };
  const built = await buildContext(c, arm, options);
  // The build wrote exactly the profile's 120 files under the three skills' directories.
  const written = dirs.flatMap((each) => {
    const skill = path.join(built.skillsDir, path.basename(each));
    return readdirSync(skill, { recursive: true, encoding: "utf8" })
      .filter((name) => statSync(path.join(skill, name)).isFile())
      .map((name) => `${each}/${name}`);
  });
  expect(written.toSorted()).toEqual(files.map((file) => file.path).toSorted());
  expect(callsOf(log)).toEqual({ tree: 1, graphql: Math.ceil(120 / 50), blob: 0, other: 0 });
  for (const file of files) {
    const at = path.join(built.skillsDir, file.path.replace(/^skills\//, ""));
    expect([...readFileSync(at)]).toEqual([...file.bytes]);
  }
  // Built again from the cache, it reads nothing.
  await buildContext(c, arm, options);
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 3, blob: 0, other: 0 });
}, 120_000);

test("a blob a batch does not give whole, binary or no UTF-8, is read alone over REST, byte for byte", async () => {
  const files: StandinFile[] = [
    {
      path: "skills/mixed/SKILL.md",
      bytes: new TextEncoder().encode("---\nname: mixed\ndescription: Made-up mixed.\n---\n"),
    },
    { path: "skills/mixed/notes.md", bytes: new TextEncoder().encode("Plain notes.\n") },
    { path: "skills/mixed/logo.bin", bytes: new Uint8Array([0, 1, 2, 255]), binary: true },
    { path: "skills/mixed/latin.txt", bytes: new Uint8Array([0x63, 0x61, 0x66, 0xe9, 0x0a]) },
  ];
  const { gh, log } = standinRepoGh("gh-mixed", files);
  const root = path.join(dir, "mixed");
  const { c, arm } = batchCase(root, ["skills/mixed"]);
  const built = await buildContext(c, arm, {
    source: ghSource(gh),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  });
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 1, blob: 2, other: 0 });
  for (const file of files) {
    const at = path.join(built.skillsDir, file.path.replace(/^skills\//, ""));
    expect([...readFileSync(at)]).toEqual([...file.bytes]);
  }
}, 120_000);

test("a tree read that failed is not kept: the next context at that commit reads it again, and the source counts each gh call", async () => {
  const bytes = new TextEncoder().encode("---\nname: again\ndescription: Made-up again.\n---\n");
  const inner = standinRepoGh("gh-again-inner", [{ path: "skills/again/SKILL.md", bytes }]);
  const root = path.join(dir, "again");
  const failedOnce = path.join(root, "tree-failed-once");
  // A gh whose first tree read fails for good (a 404); every other call goes to the stand-in.
  writeTree(root, {
    gh: `#!/bin/sh
case "$*" in *git/trees/*)
  if [ ! -e ${JSON.stringify(failedOnce)} ]; then
    : > ${JSON.stringify(failedOnce)}
    echo 'gh: Not Found (HTTP 404)' >&2
    exit 1
  fi;;
esac
exec ${JSON.stringify(inner.gh)} "$@"
`,
  });
  chmodSync(path.join(root, "gh"), 0o755);
  const { c, arm } = batchCase(root, ["skills/again"]);
  const source = ghSource(path.join(root, "gh"));
  const options = { source, cacheDir: path.join(root, "cache"), root: path.join(root, "scratch") };
  const first = await buildContext(c, arm, options).then(
    () => "built",
    (error: unknown) => String(error)
  );
  expect(first).toContain("gh: Not Found (HTTP 404)");
  const built = await buildContext(c, arm, options);
  expect([...readFileSync(path.join(built.skillsDir, "again/SKILL.md"))]).toEqual([...bytes]);
  expect(callsOf(inner.log)).toEqual({ tree: 1, graphql: 1, blob: 0, other: 0 });
  // The failed tree read, the tree read again and one GraphQL batch.
  expect(source.reads?.().calls).toBe(3);
}, 120_000);

/** `value`'s UTF-8 bytes. */
const text = (value: string) => new TextEncoder().encode(value);

/** A made-up skill's SKILL.md at `dir`, and one reference beside it. */
const skillFiles = (dir: string): StandinFile[] => [
  {
    path: `${dir}/SKILL.md`,
    bytes: text(`---\nname: ${path.basename(dir)}\ndescription: Made-up ${dir}.\n---\n`),
  },
  { path: `${dir}/notes.md`, bytes: text(`Notes of ${dir}.\n`) },
];

// Catches: a blob read ahead and held for the life of the source when its skill's files are never
// written (the build stops first, or another capture made the skill's cache entry first), or
// dropped before its last file is written (a skill directory inside another).
test("each blob read ahead is dropped once its last file is written, or once the context's skills are installed or it fails, on every path", async () => {
  // The build stops between the read ahead and the second skill's files: two skills named dup.
  const twice = standinRepoGh("gh-held-twice", [
    ...skillFiles("skills/dup"),
    ...skillFiles("other/dup"),
  ]);
  const twiceSource = ghSource(twice.gh);
  const twiceRoot = path.join(dir, "held-twice");
  const { c: twiceCase, arm: twiceArm } = batchCase(twiceRoot, ["skills/dup", "other/dup"]);
  const stopped = await buildContext(twiceCase, twiceArm, {
    source: twiceSource,
    cacheDir: path.join(twiceRoot, "cache"),
    root: path.join(twiceRoot, "scratch"),
  }).then(() => "built", String);
  expect(stopped).toContain("installs dup twice");
  expect(twiceSource.held()).toBe(0);

  // Another capture makes the skill's cache entry while this one reads ahead, so its tree never
  // runs here.
  const madeRoot = path.join(dir, "held-made");
  const made = standinRepoGh("gh-held-made-inner", skillFiles("skills/made"));
  const entry = path.join(
    madeRoot,
    "cache",
    "skills",
    cacheKey("acme/batch", COMMIT, "skills/made")
  );
  const staged = path.join(madeRoot, "staged");
  writeTree(staged, { "SKILL.md": "---\nname: made\ndescription: Made-up made.\n---\n" });
  writeTree(madeRoot, {
    gh: `#!/bin/sh
case "$*" in *graphql*)
  if [ ! -e ${JSON.stringify(entry)} ]; then
    mkdir -p ${JSON.stringify(path.dirname(entry))}
    cp -R ${JSON.stringify(staged)} ${JSON.stringify(entry)}
  fi;;
esac
exec ${JSON.stringify(made.gh)} "$@"
`,
  });
  chmodSync(path.join(madeRoot, "gh"), 0o755);
  const madeSource = ghSource(path.join(madeRoot, "gh"));
  const { c: madeCase, arm: madeArm } = batchCase(madeRoot, ["skills/made"]);
  const built = await buildContext(madeCase, madeArm, {
    source: madeSource,
    cacheDir: path.join(madeRoot, "cache"),
    root: path.join(madeRoot, "scratch"),
  });
  expect(built.record.builds.find((each) => each.kind === "skill")?.hit).toBe(true);
  expect(madeSource.held()).toBe(0);

  // A skill directory inside another: each file of the inner one is written twice, once for each.
  const nested = standinRepoGh("gh-held-nested", [
    ...skillFiles("skills/outer"),
    ...skillFiles("skills/outer/inner"),
  ]);
  const nestedSource = ghSource(nested.gh);
  const nestedRoot = path.join(dir, "held-nested");
  const { c: nestedCase, arm: nestedArm } = batchCase(nestedRoot, [
    "skills/outer",
    "skills/outer/inner",
  ]);
  await buildContext(nestedCase, nestedArm, {
    source: nestedSource,
    cacheDir: path.join(nestedRoot, "cache"),
    root: path.join(nestedRoot, "scratch"),
  });
  // Each blob came from the batch: none was read alone because it was dropped too soon.
  expect(callsOf(nested.log)).toEqual({ tree: 1, graphql: 1, blob: 0, other: 0 });
  expect(nestedSource.held()).toBe(0);
}, 120_000);

// Catches: one transport error on a GitHub read failing the context, where copies.ts sends the
// read again (a build-cases re-profile failed so on 2026-11-21).
test("a tree read that fails once with a TLS handshake timeout is sent again 5 s later, as copies.ts's reads are, and the context record and the source count the retry", async () => {
  const bytes = new TextEncoder().encode("---\nname: flaky\ndescription: Made-up flaky.\n---\n");
  const inner = standinRepoGh("gh-flaky-inner", [{ path: "skills/flaky/SKILL.md", bytes }]);
  const root = path.join(dir, "flaky");
  const failedOnce = path.join(root, "tree-failed-once");
  const timeout = `Get "https://api.example.test/repos/acme/batch/git/trees/${COMMIT}?recursive=1": net/http: TLS handshake timeout`;
  // A gh whose first tree read fails as the real read did; every other call goes to the stand-in.
  writeTree(root, {
    gh: `#!/bin/sh
case "$*" in *git/trees/*)
  if [ ! -e ${JSON.stringify(failedOnce)} ]; then
    : > ${JSON.stringify(failedOnce)}
    echo ${JSON.stringify(timeout)} >&2
    exit 1
  fi;;
esac
exec ${JSON.stringify(inner.gh)} "$@"
`,
  });
  chmodSync(path.join(root, "gh"), 0o755);
  const { c, arm } = batchCase(root, ["skills/flaky"]);
  const waits: number[] = [];
  const said: string[] = [];
  const source = ghSource(path.join(root, "gh"), {
    sleep: async (ms) => void waits.push(ms),
    err: (line) => void said.push(line),
  });
  const built = await buildContext(c, arm, {
    source,
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  });
  expect([...readFileSync(path.join(built.skillsDir, "flaky/SKILL.md"))]).toEqual([...bytes]);
  expect(waits).toEqual([5_000]);
  expect(said).toEqual([
    `context: a gh call failed (${timeout}); sending it again in 5 s, retry 1 of 4`,
  ]);
  expect(built.record.githubRetries).toEqual({ "TLS handshake timeout": 1 });
  // The failed tree read, the tree read again and one GraphQL batch.
  expect(source.reads?.()).toMatchObject({ calls: 3, retries: { "TLS handshake timeout": 1 } });
}, 120_000);

/** A file of the made-up acme/deep: its path, its mode (git's: 100755 executable, 120000 a link
 *  whose target is its text) and its bytes. */
interface DeepFile {
  readonly path: string;
  readonly mode: "100644" | "100755" | "120000";
  readonly bytes: Uint8Array;
}

/** acme/deep at COMMIT: a skill directory with an executable, a link, a nested directory and bytes
 *  that are no UTF-8, beside a sibling skill and the rest of a large repository. */
const DEEP_FILES: readonly DeepFile[] = [
  { path: "README.md", mode: "100644", bytes: text("# Deep\n") },
  { path: "data/proofs/p1.v", mode: "100644", bytes: text("Lemma one : True.\n") },
  { path: "skills/other/SKILL.md", mode: "100644", bytes: text("---\nname: other\n---\n") },
  {
    path: "skills/deep/SKILL.md",
    mode: "100644",
    bytes: text("---\nname: deep\ndescription: Made-up deep.\n---\n"),
  },
  { path: "skills/deep/scripts/run.sh", mode: "100755", bytes: text("#!/bin/sh\necho run\n") },
  { path: "skills/deep/link.md", mode: "120000", bytes: text("SKILL.md") },
  { path: "skills/deep/references/a/b.md", mode: "100644", bytes: text("Reference b.\n") },
  { path: "skills/deep/latin.bin", mode: "100644", bytes: new Uint8Array([0, 0x63, 0xe9, 255]) },
];

/** The made-up id of the tree of `dir` ("" the root) in the stand-in. */
const treeId = (dir: string) => gitBlobId(text(`tree ${dir}`));

/**
 * A stand-in `gh` holding one made-up repository (`files`) at COMMIT under any name, whose recursive tree GitHub cuts short, a
 * Bun script that logs each call's arguments as one JSON line to its `log` and answers as GitHub
 * does: a tree's recursive listing, cut short with its first entry only for the commit and for
 * each directory of `cutShort`; one level of a tree, cut short so for each directory of
 * `cutAlone`; a blob alone; and the repository's tarball, made with `tar` from the files, so a
 * tarball read gives the same files and only the call log tells it apart.
 */
function standinDeepGh(
  name: string,
  files: readonly DeepFile[],
  cut: { readonly cutShort?: readonly string[]; readonly cutAlone?: readonly string[] } = {}
): { gh: string; log: string } {
  const root = path.join(dir, name);
  const log = path.join(root, "calls.jsonl");
  const gh = path.join(root, "gh");
  // Each tree's one level, by its id.
  const trees: Record<
    string,
    { name: string; mode: string; type: string; sha: string; size?: number }[]
  > = { [treeId("")]: [] };
  const blobs: Record<string, string> = {};
  const top = path.join(root, "tarball", `acme-deep-${COMMIT.slice(0, 7)}`);
  for (const file of files) {
    const parts = file.path.split("/");
    for (let depth = 1; depth <= parts.length; depth++) {
      const parent = treeId(parts.slice(0, depth - 1).join("/"));
      const here = parts.slice(0, depth).join("/");
      const level = trees[parent] ?? [];
      trees[parent] = level;
      if (level.some((entry) => entry.name === parts[depth - 1])) continue;
      if (depth < parts.length) {
        level.push({
          name: parts[depth - 1] ?? "",
          mode: "040000",
          type: "tree",
          sha: treeId(here),
        });
        trees[treeId(here)] ??= [];
      } else {
        const sha = gitBlobId(file.bytes);
        level.push({
          name: parts[depth - 1] ?? "",
          mode: file.mode,
          type: "blob",
          sha,
          size: file.bytes.length,
        });
        blobs[sha] = Buffer.from(file.bytes).toString("base64");
      }
    }
    const at = path.join(top, file.path);
    mkdirSync(path.dirname(at), { recursive: true });
    if (file.mode === "120000") symlinkSync(new TextDecoder().decode(file.bytes), at);
    else {
      writeFileSync(at, file.bytes);
      if (file.mode === "100755") chmodSync(at, 0o755);
    }
  }
  const archive = path.join(root, "repository.tar.gz");
  const tar = Bun.spawnSync(["tar", "-czf", archive, "-C", path.dirname(top), path.basename(top)]);
  if (tar.exitCode !== 0) throw new Error(`tar failed: ${tar.stderr.toString()}`);
  const data = {
    commit: COMMIT,
    root: treeId(""),
    trees,
    blobs,
    cutShort: (cut.cutShort ?? []).map(treeId),
    cutAlone: (cut.cutAlone ?? []).map(treeId),
  };
  writeTree(root, {
    gh: `#!${process.execPath}
import { appendFileSync, readFileSync } from "node:fs";
const data = ${JSON.stringify(data)};
const args = process.argv.slice(2);
appendFileSync(${JSON.stringify(log)}, JSON.stringify(args) + "\\n");
const route = args.at(-1) ?? "";
const notFound = () => {
  process.stderr.write("gh: Not Found (HTTP 404)\\n");
  process.exit(1);
};
const listed = (entry, at) => ({ path: at, mode: entry.mode, type: entry.type, sha: entry.sha, ...(entry.size === undefined ? {} : { size: entry.size }) });
const tree = /^repos\\/[^/]+\\/[^/]+\\/git\\/trees\\/([0-9a-f]{40})(\\?recursive=1)?$/.exec(route);
const blob = /^repos\\/[^/]+\\/[^/]+\\/git\\/blobs\\/([0-9a-f]{40})$/.exec(route);
if (tree !== null) {
  const id = tree[1] === data.commit ? data.root : tree[1];
  const level = data.trees[id];
  if (level === undefined) notFound();
  if (tree[2] !== undefined) {
    const all = [];
    const walk = (sha, prefix) => {
      for (const entry of data.trees[sha]) {
        all.push(listed(entry, prefix + entry.name));
        if (entry.type === "tree") walk(entry.sha, prefix + entry.name + "/");
      }
    };
    walk(id, "");
    const truncated = tree[1] === data.commit || data.cutShort.includes(id);
    process.stdout.write(JSON.stringify({ sha: id, tree: truncated ? all.slice(0, 1) : all, truncated }));
  } else {
    const truncated = data.cutAlone.includes(id);
    const entries = level.map((entry) => listed(entry, entry.name));
    process.stdout.write(JSON.stringify({ sha: id, tree: truncated ? entries.slice(0, 1) : entries, truncated }));
  }
} else if (blob !== null && data.blobs[blob[1]] !== undefined) {
  process.stdout.write(Buffer.from(data.blobs[blob[1]], "base64"));
} else if (/^repos\\/[^/]+\\/[^/]+\\/tarball\\/[0-9a-f]{40}$/.test(route) && route.endsWith(data.commit)) {
  process.stdout.write(readFileSync(${JSON.stringify(archive)}));
} else notFound();
`,
  });
  chmodSync(gh, 0o755);
  return { gh, log };
}

/** The route of each call the stand-in logged, in order. */
function routesOf(log: string): string[] {
  return readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => (JSON.parse(line) as string[]).at(-1) ?? "");
}

/** The routes of `log` by kind: tree reads (recursive and one level), blobs, tarballs, other. */
function routeKinds(log: string): Record<string, number> {
  const kinds = { recursive: 0, level: 0, blob: 0, tarball: 0, other: 0 };
  for (const route of routesOf(log)) {
    if (/\/git\/trees\/[0-9a-f]{40}\?recursive=1$/.test(route)) kinds.recursive++;
    else if (/\/git\/trees\/[0-9a-f]{40}$/.test(route)) kinds.level++;
    else if (route.includes("/git/blobs/")) kinds.blob++;
    else if (route.includes("/tarball/")) kinds.tarball++;
    else kinds.other++;
  }
  return kinds;
}

/** Each file under `root`, by its path below it: its mode as git writes it (120000 a link, its
 *  bytes the target) and its bytes. */
function filesUnder(root: string): Record<string, { mode: string; bytes: number[] }> {
  const found: Record<string, { mode: string; bytes: number[] }> = {};
  for (const name of readdirSync(root, { recursive: true, encoding: "utf8" })) {
    const at = path.join(root, name);
    const stat = lstatSync(at);
    if (stat.isDirectory()) continue;
    found[name] = stat.isSymbolicLink()
      ? { mode: "120000", bytes: [...text(readlinkSync(at))] }
      : { mode: (stat.mode & 0o111) !== 0 ? "100755" : "100644", bytes: [...readFileSync(at)] };
  }
  return found;
}

/** What `filesUnder` gives for the files of `files` under `inside`. */
function expectedUnder(
  files: readonly DeepFile[],
  inside: string
): Record<string, { mode: string; bytes: number[] }> {
  return Object.fromEntries(
    files
      .filter((file) => file.path.startsWith(`${inside}/`))
      .map((file) => [
        file.path.slice(inside.length + 1),
        { mode: file.mode, bytes: [...file.bytes] },
      ])
  );
}

// Catches: a commit whose recursive tree GitHub cuts short read from the whole repository's
// tarball to write one directory (3.26 GB for one skill of a large proof repository, 2026-11-22).
test("a directory of a commit whose tree GitHub cuts short is read along its path and written with its modes, links and bytes, with no tarball read", async () => {
  const { gh, log } = standinDeepGh("gh-deep", DEEP_FILES);
  const root = path.join(dir, "deep");
  const { c, arm } = batchCase(root, ["skills/deep"]);
  const source = ghSource(gh, { sleep: async () => {}, err: () => {} });
  const options = { source, cacheDir: path.join(root, "cache"), root: path.join(root, "scratch") };
  const built = await buildContext(c, arm, options);
  expect(routeKinds(log)).toEqual({ recursive: 2, level: 2, blob: 5, tarball: 0, other: 0 });
  const key = cacheKey("acme/batch", COMMIT, "skills/deep");
  // The cache holds that one entry, and it holds exactly the directory's files.
  expect(readdirSync(path.join(options.cacheDir, "skills"))).toEqual([key]);
  expect(filesUnder(path.join(options.cacheDir, "skills", key))).toEqual(
    expectedUnder(DEEP_FILES, "skills/deep")
  );
  expect(filesUnder(path.join(built.skillsDir, "deep"))).toEqual(
    expectedUnder(DEEP_FILES, "skills/deep")
  );
  expect(source.reads?.().calls).toBe(9);
}, 120_000);

test("a directory whose own recursive tree GitHub cuts short too is walked one level at a time, with no tarball read", async () => {
  const { gh, log } = standinDeepGh("gh-deep-levels", DEEP_FILES, { cutShort: ["skills/deep"] });
  const source = ghSource(gh, { sleep: async () => {}, err: () => {} });
  const dest = path.join(dir, "deep-levels", "out", "deep");
  mkdirSync(path.dirname(dest), { recursive: true });
  await source.tree("acme/deep", COMMIT, "skills/deep", dest);
  expect(filesUnder(dest)).toEqual(expectedUnder(DEEP_FILES, "skills/deep"));
  // Nothing beside the directory is written.
  expect(readdirSync(path.dirname(dest))).toEqual(["deep"]);
  // The commit's tree and the directory's, cut short; the root and skills/ to find it; then its
  // four directories one level each: skills/deep, scripts, references and references/a.
  expect(routeKinds(log)).toEqual({ recursive: 2, level: 6, blob: 5, tarball: 0, other: 0 });
}, 120_000);

test("a directory the commit does not hold, or one a level of which GitHub cuts short even read alone, fails naming the repository, the commit and the path, with no tarball read", async () => {
  const missing = standinDeepGh("gh-deep-missing", DEEP_FILES);
  const missingSource = ghSource(missing.gh, { sleep: async () => {}, err: () => {} });
  const out = path.join(dir, "deep-missing");
  mkdirSync(out, { recursive: true });
  for (const inside of ["skills/none", "skills/deep/SKILL.md", "nowhere/deep"]) {
    const failed = await missingSource.tree("acme/deep", COMMIT, inside, path.join(out, "x")).then(
      () => "written",
      (error: unknown) => error
    );
    expect(failed).toBeInstanceOf(UnreadableDirectory);
    expect(failed).toMatchObject({ repo: "acme/deep", commit: COMMIT, dir: inside });
    expect(String(failed)).toContain(`acme/deep at ${COMMIT} holds no directory ${inside}`);
  }
  expect(routeKinds(missing.log).tarball).toBe(0);
  expect(existsSync(path.join(out, "x"))).toBe(false);

  const cut = standinDeepGh("gh-deep-cut", DEEP_FILES, {
    cutShort: ["skills/deep"],
    cutAlone: ["skills/deep/references"],
  });
  const cutSource = ghSource(cut.gh, { sleep: async () => {}, err: () => {} });
  const cutOut = path.join(dir, "deep-cut");
  mkdirSync(cutOut, { recursive: true });
  const failed = await cutSource
    .tree("acme/deep", COMMIT, "skills/deep", path.join(cutOut, "x"))
    .then(
      () => "written",
      (error: unknown) => error
    );
  expect(failed).toBeInstanceOf(UnreadableDirectory);
  expect(failed).toMatchObject({ repo: "acme/deep", commit: COMMIT, dir: "skills/deep" });
  expect(String(failed)).toContain(
    `acme/deep at ${COMMIT}: skills/deep cannot be read: GitHub cuts the tree of skills/deep/references short even one level at a time`
  );
  expect(routeKinds(cut.log).tarball).toBe(0);
}, 120_000);

/** What `filesUnder` gives for each of `files`, at its whole path. */
function expectedAll(
  files: readonly DeepFile[]
): Record<string, { mode: string; bytes: number[] }> {
  return Object.fromEntries(
    files.map((file) => [file.path, { mode: file.mode, bytes: [...file.bytes] }])
  );
}

/** A case of alpha whose profile also installs the skill at the root of acme/batch as `rooted`,
 *  and its old arm, under `root`. */
function rootedCase(root: string): { c: Case; arm: Arm } {
  const c = aCase("devbox", {
    skills: [
      { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" },
      { repo: "acme/batch", commit: COMMIT, path: ".", name: "rooted" },
    ],
    contextFiles: [],
  });
  writeTree(path.join(root, "old"), { "SKILL.md": ALPHA_ARM });
  const arm = buildArm(
    c,
    "old",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  return { c, arm };
}

// Catches: a skill at its repository's root (a profile path of ".", build.ts) read as a
// directory named "." that the commit does not hold, or from the whole repository's tarball.
test("a skill at its repository's root is the root's files at their whole paths, read from the commit's tree and read ahead, or walked from the root where GitHub cuts the tree short, with no tarball read", async () => {
  const files: StandinFile[] = [
    { path: "SKILL.md", bytes: text("---\nname: rooted\ndescription: Made-up rooted.\n---\n") },
    { path: "references/r.md", bytes: text("Reference of the root skill.\n") },
  ];
  const { gh, log } = standinRepoGh("gh-root", files);
  const root = path.join(dir, "root-skill");
  const { c, arm } = rootedCase(root);
  const built = await buildContext(c, arm, {
    source: ghSource(gh, { sleep: async () => {}, err: () => {} }),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  });
  for (const file of files) {
    expect([...readFileSync(path.join(built.skillsDir, "rooted", file.path))]).toEqual([
      ...file.bytes,
    ]);
  }
  expect(built.skills.map((skill) => skill.name)).toEqual(["alpha", "rooted"]);
  // One tree read and one GraphQL batch; no blob alone, no tarball (`other`).
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 1, blob: 0, other: 0 });

  const deep = standinDeepGh("gh-root-deep", DEEP_FILES);
  const dest = path.join(dir, "root-deep", "out", "rooted");
  mkdirSync(path.dirname(dest), { recursive: true });
  await ghSource(deep.gh, { sleep: async () => {}, err: () => {} }).tree(
    "acme/deep",
    COMMIT,
    ".",
    dest
  );
  expect(filesUnder(dest)).toEqual(expectedAll(DEEP_FILES));
  // The commit's tree, cut short, then each of the root's nine levels alone: the root, data,
  // data/proofs, skills, skills/other, skills/deep and its scripts, references and references/a.
  expect(routeKinds(deep.log)).toEqual({ recursive: 1, level: 9, blob: 8, tarball: 0, other: 0 });
}, 120_000);

// Catches: a "skill" at the root of a large repository read whole, file by file.
test("a skill at a root of more than 200 files is refused, naming the repository, the commit and the count, with no blob or tarball read, from the commit's tree or a walk cut short", async () => {
  const many: StandinFile[] = [
    { path: "SKILL.md", bytes: text("---\nname: rooted\ndescription: Made-up rooted.\n---\n") },
    ...Array.from({ length: 200 }, (_, index) => ({
      path: `notes/n${index}.md`,
      bytes: text(`Note ${index}.\n`),
    })),
  ];
  const { gh, log } = standinRepoGh("gh-root-large", many);
  const root = path.join(dir, "root-large");
  const { c, arm } = rootedCase(root);
  const failed = await buildContext(c, arm, {
    source: ghSource(gh, { sleep: async () => {}, err: () => {} }),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  }).then(
    () => "built",
    (error: unknown) => error
  );
  expect(failed).toBeInstanceOf(UnreadableDirectory);
  expect(failed).toMatchObject({ repo: "acme/batch", commit: COMMIT, dir: "." });
  expect(String(failed)).toContain(
    `acme/batch at ${COMMIT}: its root holds 201 files, more than the 200 a skill at a repository's root is read with`
  );
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 0, blob: 0, other: 0 });

  const deep = standinDeepGh(
    "gh-root-deep-large",
    many.map((file) => ({ path: file.path, mode: "100644" as const, bytes: file.bytes }))
  );
  const walked = await ghSource(deep.gh, { sleep: async () => {}, err: () => {} })
    .tree("acme/deep", COMMIT, ".", path.join(dir, "root-deep-large", "x"))
    .then(
      () => "written",
      (error: unknown) => error
    );
  expect(walked).toBeInstanceOf(UnreadableDirectory);
  expect(String(walked)).toContain(
    `acme/deep at ${COMMIT}: its root holds 201 files, more than the 200 a skill at a repository's root is read with`
  );
  // The commit's tree, cut short, then the root and notes/, one level each: no blob read.
  expect(routeKinds(deep.log)).toEqual({ recursive: 1, level: 2, blob: 0, tarball: 0, other: 0 });
  expect(existsSync(path.join(dir, "root-deep-large", "x"))).toBe(false);
}, 120_000);

/** A made-up skill of acme/batch, at `skills/linked` and, as a project's skills directory holds
 *  one, at `.claude/skills/linked`, a link to it. */
const LINKED_FILES: readonly StandinFile[] = [
  {
    path: "skills/linked/SKILL.md",
    bytes: text("---\nname: linked\ndescription: Made-up linked.\n---\n"),
  },
  { path: "skills/linked/references/r.md", bytes: text("Reference of linked.\n") },
  { path: ".claude/skills/linked", bytes: text("../../skills/linked"), mode: "120000" },
];

// Catches: a project skill whose directory the repository holds as a symbolic link, which build.ts
// names by the link's path (where Oh My Pi lists it), read as a directory that holds no file, so
// every case of a profile that installs one is dropped.
test("a skill directory the repository holds as a link is installed from the directory the link names, from the commit's tree, and from a walk where GitHub cuts the tree short", async () => {
  const { gh, log } = standinRepoGh("gh-linked", LINKED_FILES);
  const root = path.join(dir, "linked");
  const { c, arm } = batchCase(root, [".claude/skills/linked"]);
  const options = {
    source: ghSource(gh, { sleep: async () => {}, err: () => {} }),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  };
  const built = await buildContext(c, arm, options);
  const installed = path.join(built.skillsDir, "linked");
  expect(filesUnder(installed)).toEqual({
    "SKILL.md": { mode: "100644", bytes: [...(LINKED_FILES[0]?.bytes ?? [])] },
    "references/r.md": { mode: "100644", bytes: [...(LINKED_FILES[1]?.bytes ?? [])] },
  });
  expect(built.skills.find((skill) => skill.name === "linked")?.description).toBe(
    "Made-up linked."
  );
  expect(built.record.skills.find((skill) => skill.name === "linked")?.from).toEqual({
    repo: "acme/batch",
    commit: COMMIT,
    path: ".claude/skills/linked",
    key: cacheKey("acme/batch", COMMIT, ".claude/skills/linked"),
  });
  // The commit's tree, the link's text, and one GraphQL batch of the target's two files.
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 1, blob: 1, other: 0 });
  // Built again from the cache, it reads nothing.
  await buildContext(c, arm, options);
  expect(callsOf(log)).toEqual({ tree: 1, graphql: 1, blob: 1, other: 0 });

  const deep = standinDeepGh("gh-linked-deep", [
    ...DEEP_FILES,
    { path: ".claude/skills/deep", mode: "120000", bytes: text("../../skills/deep") },
  ]);
  const deepRoot = path.join(dir, "linked-deep");
  const walked = batchCase(deepRoot, [".claude/skills/deep"]);
  const deepBuilt = await buildContext(walked.c, walked.arm, {
    source: ghSource(deep.gh, { sleep: async () => {}, err: () => {} }),
    cacheDir: path.join(deepRoot, "cache"),
    root: path.join(deepRoot, "scratch"),
  });
  expect(filesUnder(path.join(deepBuilt.skillsDir, "deep"))).toEqual(
    expectedUnder(DEEP_FILES, "skills/deep")
  );
  // The commit's tree, cut short; the root, .claude and .claude/skills to find the link, and
  // skills to find that skills/deep is no link; the link's text; skills/deep's own tree, its
  // levels on the way kept from the link's read; and its five blobs.
  expect(routeKinds(deep.log)).toEqual({ recursive: 2, level: 4, blob: 6, tarball: 0, other: 0 });
}, 120_000);

// Catches: a linked skill directory that no read of the repository gives (its link leads out of
// the repository, or to no directory of it) installed as an empty directory, or failing with an
// error that names neither the case nor the link.
test("a skill directory held as a link out of the repository, or to no directory of it, is an UnreadableDirectory naming the case, the link and its target", async () => {
  const { gh } = standinRepoGh("gh-linked-away", [
    ...LINKED_FILES,
    { path: ".claude/skills/away", bytes: text("../../../outside/away"), mode: "120000" },
    { path: ".claude/skills/gone", bytes: text("../../skills/gone"), mode: "120000" },
  ]);
  const failure = async (name: string) => {
    const root = path.join(dir, `linked-${name}`);
    const { c, arm } = batchCase(root, [`.claude/skills/${name}`]);
    const failed = await buildContext(c, arm, {
      source: ghSource(gh, { sleep: async () => {}, err: () => {} }),
      cacheDir: path.join(root, "cache"),
      root: path.join(root, "scratch"),
    }).then(
      () => "built",
      (error: unknown) => error
    );
    return { c, failed };
  };
  const away = await failure("away");
  expect(away.failed).toBeInstanceOf(UnreadableDirectory);
  expect(away.failed).toMatchObject({
    repo: "acme/batch",
    commit: COMMIT,
    dir: ".claude/skills/away",
    message: `case ${away.c.id}: the profile's skill away is .claude/skills/away of acme/batch at ${COMMIT}, which holds .claude/skills/away as a symbolic link to ../../../outside/away, outside the repository, so no read of the repository gives the skill`,
  });
  const gone = await failure("gone");
  expect(gone.failed).toBeInstanceOf(UnreadableDirectory);
  expect(gone.failed).toMatchObject({
    repo: "acme/batch",
    commit: COMMIT,
    dir: "skills/gone",
    message: `case ${gone.c.id}: the profile's skill gone is .claude/skills/gone of acme/batch at ${COMMIT}, which holds it as a symbolic link (.claude/skills/gone -> ../../skills/gone) to no directory of the repository: acme/batch at ${COMMIT} holds no file under skills/gone`,
  });
}, 120_000);

/** A made-up dispatch-first, which the Envoy plugin reads as it loads. */
const DISPATCH_FIRST =
  "---\nname: dispatch-first\ndescription: Use when the made-up session has Dispatch.\n---\n\n# Dispatch first\n\nSearch before you act.\n";

// Catches: a none arm of dispatch-first that removes the file the Envoy plugin reads as it loads,
// so the plugin fails to load and every such case fails at its first run.
test("a none arm of a skill the Envoy plugin reads as it loads installs a hidden stand-in of its file, so the plugin's skills hold it and nothing more is listed; an arm with the skill installs its own copy", async () => {
  const root = path.join(dir, "stand-in");
  writeRepositories(path.join(root, "repos"));
  writeTree(path.join(root, "repos", "acme", "legion", COMMIT), {
    "packages/pi-envoy/package.json": JSON.stringify({
      name: "pi-envoy",
      files: ["dist"],
      scripts: { build: "bun build extensions/envoy.ts --outdir dist --target bun" },
      omp: { extensions: ["extensions/envoy.ts"], skills: ["dist/skills"] },
    }),
    "packages/pi-envoy/extensions/envoy.ts": "export default function plugin(): void {}\n",
  });
  writeTree(path.join(root, "repos", "acme", "skills", COMMIT), {
    "skills/dispatch-first/SKILL.md": DISPATCH_FIRST,
  });
  writeTree(path.join(root, "old"), { "SKILL.md": DISPATCH_FIRST });
  const read: SessionMessage[] = [
    { role: "user", content: [{ type: "text", text: "Plan the made-up change." }] },
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
    },
  ];
  const base = aCase("devbox", {
    skills: [
      { repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" },
      {
        repo: "acme/skills",
        commit: COMMIT,
        path: "skills/dispatch-first",
        name: "dispatch-first",
      },
    ],
    plugins: [{ repo: "acme/legion", commit: COMMIT, path: "packages/pi-envoy", name: "pi-envoy" }],
    contextFiles: [],
  });
  const c: Case = {
    ...base,
    skill: "dispatch-first",
    line: { text: "Search before you act.", lineNumber: 8, lineCount: 8 },
    messages: read,
    skillReads: [2],
  };
  const dirs = { old: path.join(root, "old"), new: path.join(root, "old") };
  const options = {
    source: localSource(path.join(root, "repos")),
    cacheDir: path.join(root, "cache"),
    root: path.join(root, "scratch"),
  };
  const none = await buildContext(c, buildArm(c, "none", dirs, root), options);
  expect(none.record.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
    ["pi-envoy", "extension"],
  ]);
  // The plugin's skills are the context's, and hold the file it reads as it loads.
  const pluginFile = path.join(none.extensions[0] ?? "", "dist/skills/dispatch-first/SKILL.md");
  expect(readFrontmatter(readFileSync(pluginFile, "utf8"))).toMatchObject({
    name: "dispatch-first",
    hide: true,
  });
  expect(none.skills.map((skill) => [skill.name, skill.hidden])).toEqual([
    ["beta", false],
    ["dispatch-first", true],
  ]);
  expect(none.record.skills.map((skill) => skill.from)).toContainEqual("stand-in");
  const old = await buildContext(c, buildArm(c, "old", dirs, root), options);
  expect(readFileSync(path.join(old.skillsDir, "dispatch-first/SKILL.md"), "utf8")).toBe(
    DISPATCH_FIRST
  );
  expect(old.skills.map((skill) => [skill.name, skill.hidden])).toEqual([
    ["beta", false],
    ["dispatch-first", false],
  ]);
  expect(old.record.skills.map((skill) => skill.from)).not.toContainEqual("stand-in");
}, 120_000);

/** A made-up plugin of agents whose `alpha-reviewer` autoloads alpha and beta, its profile entry,
 *  and that agent's definition as the record names it. */
const PLUGIN_AGENTS = {
  "packages/agents-pack/agents/reviewer.md":
    "---\nname: alpha-reviewer\ndescription: Reviews with alpha.\nautoloadSkills: [alpha, beta]\n---\n\nReview.\n",
  "packages/agents-pack/agents/listed.md":
    "---\nname: listed\ndescription: Lists.\nautoloadSkills: [beta]\n---\n\nList.\n",
};
const AGENTS_PACK = [
  { repo: "acme/legion", commit: COMMIT, path: "packages/agents-pack", name: "agents-pack" },
];
const PLUGIN_REVIEWER: AgentDefinition = {
  name: "alpha-reviewer",
  source: "plugin",
  repo: "acme/legion",
  commit: COMMIT,
  path: "packages/agents-pack/agents/reviewer.md",
  autoloadSkills: ["alpha", "beta"],
};

// Catches: a subagent case of a skill its agent autoloads, read as if only its listing line and
// its reads carried the skill.
test("a subagent's context names the agent definition of the profile's plugins it ran, with the skills its autoloadSkills names; another context names none", async () => {
  const root = path.join(dir, "agents");
  const agents = {
    "packages/agents-pack/agents/reviewer.md":
      "---\nname: alpha-reviewer\ndescription: Reviews with alpha.\nautoloadSkills: [alpha, beta]\n---\n\nReview.\n",
    "packages/agents-pack/agents/listed.md":
      "---\nname: listed\ndescription: Lists.\nautoload-skills: beta, gamma\n---\n\nList.\n",
    "packages/agents-pack/agents/nameless.md": "---\nname: nameless\n---\n\nNo description.\n",
  };
  const agentOf = async (context: string) => {
    const c = aCase(context, { plugins: AGENTS_PACK, contextFiles: [] });
    const where = path.join(root, context.replace(/\W/g, "-"));
    const { options, arm } = setUp(where, c);
    writeTree(path.join(where, "repos", "acme", "legion", COMMIT), agents);
    return (await buildContext(c, arm, options)).record.agent;
  };
  expect(await agentOf("subagent:alpha-reviewer")).toEqual(PLUGIN_REVIEWER);
  expect((await agentOf("subagent:listed"))?.autoloadSkills).toEqual(["beta", "gamma"]);
  // An agent with no description is none Oh My Pi loads; one no plugin defines is null.
  expect(await agentOf("subagent:nameless")).toBeNull();
  expect(await agentOf("subagent:task")).toBeNull();
  expect(await agentOf("devbox")).toBeNull();
}, 120_000);

/** A made-up dotfiles repository's installers, as a dev box's link its agents for Oh My Pi:
 *  installers/omp.sh links `omp/agents` as `~/.omp/agent/agents` on line 9, after a commented-out
 *  link and a link of another directory. */
const DOTFILES: Record<string, string> = {
  "installers/lib.sh": [
    "#!/bin/bash",
    `DOTFILES_DIR="\${DOTFILES_DIR:-$(cd "$(dirname "\${BASH_SOURCE[0]}")/.." && pwd)}"`,
    'ensure_link() { ln -sfn "$1" "$2"; }',
    "",
  ].join("\n"),
  "installers/omp.sh": [
    "#!/bin/bash",
    "set -euo pipefail",
    `source "$(dirname "\${BASH_SOURCE[0]}")/lib.sh"`,
    "",
    `OMP_AGENT_DIR="\${HOME}/.omp/agent"`,
    'mkdir -p "$OMP_AGENT_DIR"',
    `# ensure_link "\${DOTFILES_DIR}/omp/old-agents" "\${OMP_AGENT_DIR}/agents"`,
    `ensure_link "\${DOTFILES_DIR}/omp/hooks"       "\${OMP_AGENT_DIR}/hooks"`,
    `ensure_link "\${DOTFILES_DIR}/omp/agents"      "\${OMP_AGENT_DIR}/agents"`,
    "",
  ].join("\n"),
  "omp/agents/reviewer.md":
    "---\nname: alpha-reviewer\ndescription: Reviews, the dev box's own way.\n---\n\nReview.\n",
  "omp/hooks/post/note.ts": "export default function hook(): void {}\n",
  "skills/gamma/SKILL.md": "---\nname: gamma\ndescription: Use for the gamma notes.\n---\n",
};

/** The skill gamma of DOTFILES, as a profile names it. */
const GAMMA = { repo: "acme/dotfiles", commit: COMMIT, path: "skills/gamma", name: "gamma" };

/** The user agent of DOTFILES, as the record names it. */
const DOTFILES_REVIEWER: AgentDefinition = {
  name: "alpha-reviewer",
  source: "user",
  repo: "acme/dotfiles",
  commit: COMMIT,
  path: "omp/agents/reviewer.md",
  linkedBy: "installers/omp.sh:9",
  autoloadSkills: [],
};

/** The evidence item build.ts writes for the session of the subagent of `context`, of the store
 *  whose context is `store`. */
function subagentItem(context: string, store: string): { item: string; source: string } {
  const agent = context.slice("subagent:".length);
  return {
    item: `subagent ${agent}`,
    source: `the session's session_init line names the agent ${agent}; its parent session is another transcript, and the store's context is ${store}`,
  };
}

// Catches: a dev box's subagent read as running the agent definition of a plugin of its profile,
// though the dev box's installers link the box's own agents as Oh My Pi's user agents, which Oh
// My Pi reads first, so a plugin's agent of the same name that autoloads a skill is not the one
// the subagent ran.
test("a dev box's subagent runs the user agent its installers link as ~/.omp/agent/agents before a plugin's of the same name; a Legion subagent, or a profile of two such repositories, reads the plugins'", async () => {
  const root = path.join(dir, "user-agents");
  const delta = { repo: "acme/box", commit: COMMIT, path: "skills/delta", name: "delta" };
  const built = async (
    name: string,
    context: string,
    store: string | undefined,
    extra: (typeof GAMMA)[] = []
  ) => {
    const base = aCase(context, { plugins: AGENTS_PACK, contextFiles: [] });
    const c: Case = {
      ...base,
      id: `case-${name}`,
      profile: { ...base.profile, skills: [...base.profile.skills, GAMMA, ...extra] },
      ...(store === undefined ? {} : { evidence: [subagentItem(context, store)] }),
    };
    const where = path.join(root, name);
    const { options, arm } = setUp(where, c);
    writeTree(path.join(where, "repos", "acme", "legion", COMMIT), PLUGIN_AGENTS);
    writeTree(path.join(where, "repos", "acme", "dotfiles", COMMIT), DOTFILES);
    writeTree(path.join(where, "repos", "acme", "box", COMMIT), {
      "installers/setup.sh": `#!/bin/bash\nROOT="$(cd "$(dirname "\${BASH_SOURCE[0]}")/.." && pwd)"\nln -f "$ROOT/agents" ~/.omp/agent/agents\nln -sfn "$ROOT/agents" ~/.omp/agent/agents\n`,
      "agents/reviewer.md": "---\nname: alpha-reviewer\ndescription: The box's.\n---\n",
      "skills/delta/SKILL.md": "---\nname: delta\ndescription: Use for delta.\n---\n",
    });
    return { c, arm, options, context: await buildContext(c, arm, options) };
  };
  const devbox = await built("devbox", "subagent:alpha-reviewer", "devbox");
  expect(devbox.context.record.agent).toEqual(DOTFILES_REVIEWER);
  expect(await subagentDefinition(devbox.c, devbox.options)).toEqual(DOTFILES_REVIEWER);
  expect(devbox.context.record.leftOut).toEqual([]);
  // Each repository's installers/ and the linked omp/agents, each read once and kept: a build
  // again reads all four from the cache.
  const again = await buildContext(devbox.c, devbox.arm, devbox.options);
  expect(again.record.agent).toEqual(DOTFILES_REVIEWER);
  expect(
    again.record.builds.filter((build) => build.kind === "dir").map((build) => build.hit)
  ).toEqual([true, true, true, true]);
  // A case with no evidence of its store reads them too.
  expect(
    (await built("no-evidence", "subagent:alpha-reviewer", undefined)).context.record.agent
  ).toEqual(DOTFILES_REVIEWER);
  // An agent only the plugin defines is the plugin's.
  expect((await built("listed", "subagent:listed", "devbox")).context.record.agent).toEqual({
    name: "listed",
    source: "plugin",
    repo: "acme/legion",
    commit: COMMIT,
    path: "packages/agents-pack/agents/listed.md",
    autoloadSkills: ["beta"],
  });
  // A Legion pod runs no dev box's installers.
  const legion = await built("legion", "subagent:alpha-reviewer", "legion:acme");
  expect(legion.context.record.agent).toEqual(PLUGIN_REVIEWER);
  expect(await subagentDefinition(legion.c, legion.options)).toEqual(PLUGIN_REVIEWER);
  // Two repositories whose installers each link one: neither is read, and the record says so.
  const two = await built("two", "subagent:alpha-reviewer", "devbox", [delta]);
  expect(two.context.record.agent).toEqual(PLUGIN_REVIEWER);
  expect(two.context.record.leftOut).toEqual([
    "the user agents of a dev box: acme/dotfiles's installers/omp.sh:9 and acme/box's installers/setup.sh:4 each link a directory as Oh My Pi's user agents directory, and a dev box runs one repository's installers, so the context reads none of them",
  ]);
}, 120_000);

// Catches: a dev box's subagent whose profile holds its dotfiles at two commits, the root's last
// prompt build (its plugin lists and installers) and the subagent's start (its pool
// directories), read at the first, though Oh My Pi reads the user agents the installers link at
// the subagent's start, from the dotfiles checkout as it is then.
test("a dev box's subagent reads its user agents, and the installer line that links them, at the commit of its start that its read-times evidence names; a profile of two commits with no such evidence reads none", async () => {
  const root = path.join(dir, "user-agents-at-start");
  const START = "fedcba9876543210fedcba9876543210fedcba98";
  // At the root's build the box's alpha-reviewer autoloads alpha; by the subagent's start the
  // installer has a line more and the agent autoloads nothing.
  const atBuild = {
    ...DOTFILES,
    "omp/agents/reviewer.md":
      "---\nname: alpha-reviewer\ndescription: Reviews, as the box did.\nautoloadSkills: [alpha]\n---\n",
  };
  const atStart = {
    ...DOTFILES,
    "installers/omp.sh": (DOTFILES["installers/omp.sh"] ?? "").replace(
      "set -euo pipefail\n",
      "set -euo pipefail\n# The agents are this repository's.\n"
    ),
    "omp/agents/reviewer.md":
      "---\nname: alpha-reviewer\ndescription: Reviews, as the box does now.\n---\n",
    "skills/epsilon/SKILL.md": "---\nname: epsilon\ndescription: Use for the epsilon pool.\n---\n",
  };
  // GAMMA is a unit of the root's build, epsilon a skill of a pool at the subagent's start.
  const epsilon = { repo: "acme/dotfiles", commit: START, path: "skills/epsilon", name: "epsilon" };
  const readTimes = {
    item: "read times of acme/dotfiles",
    source: `its plugin lists, installers and settings at ${COMMIT} (the root's last prompt build 2026-11-20T00:59:00Z), its pool directories at ${START}, the last commit of main at or before the subagent's start 2026-11-20T01:00:00Z: Oh My Pi 18.18.6 gives a subagent the skills its parent loaded`,
  };
  const built = async (name: string, evidence: Case["evidence"]) => {
    const base = aCase("subagent:alpha-reviewer", { plugins: AGENTS_PACK, contextFiles: [] });
    const c: Case = {
      ...base,
      id: `case-${name}`,
      profile: { ...base.profile, skills: [...base.profile.skills, GAMMA, epsilon] },
      evidence,
    };
    const where = path.join(root, name);
    const { options, arm } = setUp(where, c);
    writeTree(path.join(where, "repos", "acme", "legion", COMMIT), PLUGIN_AGENTS);
    writeTree(path.join(where, "repos", "acme", "dotfiles", COMMIT), atBuild);
    writeTree(path.join(where, "repos", "acme", "dotfiles", START), atStart);
    return { c, options, context: await buildContext(c, arm, options) };
  };
  const subagent = subagentItem("subagent:alpha-reviewer", "devbox");
  const userAgent: AgentDefinition = {
    name: "alpha-reviewer",
    source: "user",
    repo: "acme/dotfiles",
    commit: START,
    path: "omp/agents/reviewer.md",
    linkedBy: "installers/omp.sh:10",
    autoloadSkills: [],
  };
  const timed = await built("timed", [subagent, readTimes]);
  expect(timed.context.record.agent).toEqual(userAgent);
  expect(await subagentDefinition(timed.c, timed.options)).toEqual(userAgent);
  expect(timed.context.record.leftOut).toEqual([]);
  // With no read-times item, which of the two commits the box was at is not known.
  const untimed = await built("untimed", [subagent]);
  expect(untimed.context.record.agent).toEqual(PLUGIN_REVIEWER);
  expect(untimed.context.record.leftOut).toEqual([
    `the user agents of a dev box: the profile holds acme/dotfiles at ${COMMIT} and ${START}, its installers/omp.sh:9 at ${COMMIT} and installers/omp.sh:10 at ${START} each link a directory as Oh My Pi's user agents directory, and no evidence item "read times of acme/dotfiles" names the commit of the subagent's start, when Oh My Pi reads that directory, so the context reads none`,
  ]);
}, 120_000);

/** A made-up session repository's project agents: `alpha-reviewer` in the `.omp/agents` of its
 *  directory agents/vm (autoloading beta) and of its root (autoloading alpha), and a
 *  `.omp/agents` of its directory tools that holds no agent. */
const PROJECT_AGENT_FILES = {
  "agents/vm/.omp/agents/reviewer.md":
    "---\nname: alpha-reviewer\ndescription: Reviews, the project's way for the VM.\nautoloadSkills: [beta]\n---\n",
  ".omp/agents/reviewer.md":
    "---\nname: alpha-reviewer\ndescription: Reviews, the project's way.\nautoloadSkills: [alpha]\n---\n",
  "tools/.omp/agents/README.txt": "The tools' agents go here.\n",
};

/** The project agent `alpha-reviewer` of acme/project at `file`, as the record names it,
 *  autoloading `autoloadSkills`, reached through the links `linkedBy` names when it is given. */
const projectAgent = (
  file: string,
  autoloadSkills: string[],
  linkedBy?: string
): AgentDefinition => ({
  name: "alpha-reviewer",
  source: "project",
  repo: "acme/project",
  commit: COMMIT,
  path: file,
  ...(linkedBy === undefined ? {} : { linkedBy }),
  autoloadSkills,
});

// Catches: a subagent read as running the dev box's user agent or a plugin's agent of its name,
// though the session repository's `.omp/agents` nearest its working directory defines one, which
// Oh My Pi reads before both.
test("a subagent runs the project agent of its name from the nearest .omp/agents of its session repository, from the working directory up, before the user's and a plugin's; a nearer .omp/agents with no agent hides a farther one; no session repository reads none", async () => {
  const root = path.join(dir, "project-agents");
  const project = { repo: "acme/project", commit: COMMIT };
  const built = async (name: string, profile: Partial<Case["profile"]>, store = "devbox") => {
    const base = aCase("subagent:alpha-reviewer", { plugins: AGENTS_PACK, contextFiles: [] });
    const c: Case = {
      ...base,
      id: `case-${name}`,
      profile: { ...base.profile, skills: [...base.profile.skills, GAMMA], ...profile },
      evidence: [subagentItem("subagent:alpha-reviewer", store)],
    };
    const where = path.join(root, name);
    const { options, arm } = setUp(where, c);
    writeTree(path.join(where, "repos", "acme", "legion", COMMIT), PLUGIN_AGENTS);
    writeTree(path.join(where, "repos", "acme", "dotfiles", COMMIT), DOTFILES);
    writeTree(path.join(where, "repos", "acme", "project", COMMIT), PROJECT_AGENT_FILES);
    return { c, options, context: await buildContext(c, arm, options) };
  };
  // From agents/vm, its own .omp/agents, not the root's.
  const vm = await built("vm", { sessionRepository: project, workingDirectory: "agents/vm" });
  expect(vm.context.record.agent).toEqual(
    projectAgent("agents/vm/.omp/agents/reviewer.md", ["beta"])
  );
  expect(await subagentDefinition(vm.c, vm.options)).toEqual(
    projectAgent("agents/vm/.omp/agents/reviewer.md", ["beta"])
  );
  // From a directory with none above it but the root's, the root's.
  expect(
    (await built("docs", { sessionRepository: project, workingDirectory: "docs/guide" })).context
      .record.agent
  ).toEqual(projectAgent(".omp/agents/reviewer.md", ["alpha"]));
  expect((await built("root", { sessionRepository: project })).context.record.agent).toEqual(
    projectAgent(".omp/agents/reviewer.md", ["alpha"])
  );
  // tools/.omp/agents is the nearest, and Oh My Pi reads it alone: it defines no agent, so the
  // root's is not read and the user agent is the one.
  expect(
    (await built("tools", { sessionRepository: project, workingDirectory: "tools" })).context.record
      .agent
  ).toEqual(DOTFILES_REVIEWER);
  // A Legion pod's checkout has its project agents too.
  expect(
    (
      await built(
        "legion",
        { sessionRepository: project, workingDirectory: "agents/vm" },
        "legion:acme"
      )
    ).context.record.agent
  ).toEqual(projectAgent("agents/vm/.omp/agents/reviewer.md", ["beta"]));
  // A profile that names no session repository reads no project agents.
  expect((await built("none", { workingDirectory: "agents/vm" })).context.record.agent).toEqual(
    DOTFILES_REVIEWER
  );
}, 120_000);

/** A made-up subagent case of alpha-reviewer for the project agents' rows: its profile names
 *  AGENTS_PACK, GAMMA (whose repository's installers link the user agents) and `profile`,
 *  and its evidence is the subagent's of a dev box, then `evidence`. */
function projectCase(
  name: string,
  profile: Partial<Case["profile"]>,
  evidence: NonNullable<Case["evidence"]> = []
): Case {
  const base = aCase("subagent:alpha-reviewer", { plugins: AGENTS_PACK, contextFiles: [] });
  return {
    ...base,
    id: `case-${name}`,
    profile: { ...base.profile, skills: [...base.profile.skills, GAMMA], ...profile },
    evidence: [subagentItem("subagent:alpha-reviewer", "devbox"), ...evidence],
  };
}

/** The record's agent and leftOut of `c`'s context, its made-up repositories under `root`: the
 *  agents pack, `dotfiles` as acme/dotfiles, and `project` (files, and links by path to target)
 *  in acme/project over the usual ones. */
async function projectContext(
  root: string,
  c: Case,
  dotfiles: Record<string, string>,
  project: { readonly files: Record<string, string>; readonly links?: Record<string, string> }
): Promise<{ agent: AgentDefinition | null; leftOut: readonly string[] }> {
  const { options, arm } = setUp(root, c);
  const repos = path.join(root, "repos", "acme");
  writeTree(path.join(repos, "legion", COMMIT), PLUGIN_AGENTS);
  writeTree(path.join(repos, "dotfiles", COMMIT), dotfiles);
  writeTree(path.join(repos, "project", COMMIT), project.files);
  for (const [link, target] of Object.entries(project.links ?? {})) {
    const at = path.join(repos, "project", COMMIT, link);
    mkdirSync(path.dirname(at), { recursive: true });
    symlinkSync(target, at);
  }
  const { record } = await buildContext(c, arm, options);
  return { agent: record.agent, leftOut: record.leftOut };
}

// Catches: a session repository whose `.omp/agents`, or its `.omp`, is a symbolic link read as
// holding no project agents, though Oh My Pi's stat follows the link; and a link out of the
// repository passed over in silence.
test("a .omp/agents or .omp the session repository holds as a link is followed to its target in the repository; a link out of it is recorded and stops the walk; a link to nothing is passed", async () => {
  const root = path.join(dir, "project-links");
  const project = {
    files: {
      "config/agents/reviewer.md":
        "---\nname: alpha-reviewer\ndescription: Reviews, from the linked agents.\nautoloadSkills: [beta]\n---\n",
      "shared/omp/agents/reviewer.md":
        "---\nname: alpha-reviewer\ndescription: Reviews, from the linked .omp.\nautoloadSkills: [alpha]\n---\n",
      ".omp/agents/reviewer.md":
        "---\nname: alpha-reviewer\ndescription: Reviews, the root's way.\nautoloadSkills: [gamma]\n---\n",
    },
    links: {
      "agents/vm/.omp/agents": "../../../config/agents",
      "web/.omp": "../shared/omp",
      "ops/.omp/agents": "/etc/omp/agents",
      "gone/.omp/agents": "../missing",
    },
  };
  const sessionRepository = { repo: "acme/project", commit: COMMIT };
  const at = (workingDirectory: string) =>
    projectContext(
      path.join(root, workingDirectory.replace(/\W/g, "-")),
      projectCase(workingDirectory, { sessionRepository, workingDirectory }),
      DOTFILES,
      project
    );
  expect(await at("agents/vm")).toEqual({
    agent: projectAgent(
      "config/agents/reviewer.md",
      ["beta"],
      "agents/vm/.omp/agents -> ../../../config/agents"
    ),
    leftOut: [],
  });
  expect(await at("web")).toEqual({
    agent: projectAgent("shared/omp/agents/reviewer.md", ["alpha"], "web/.omp -> ../shared/omp"),
    leftOut: [],
  });
  // Out of the repository: neither its target nor the root's .omp/agents is read.
  expect(await at("ops")).toEqual({
    agent: DOTFILES_REVIEWER,
    leftOut: [
      `the project agents: the session repository acme/project at ${COMMIT} holds ops/.omp/agents as a link to /etc/omp/agents, outside the repository, so the context reads neither its target, which Oh My Pi reads as the project's agents when it is a directory, nor any .omp/agents farther from the working directory`,
    ],
  });
  // A link to nothing in the repository is no directory: the walk goes on to the root's.
  expect(await at("gone")).toEqual({
    agent: projectAgent(".omp/agents/reviewer.md", ["gamma"]),
    leftOut: [],
  });
}, 120_000);

/** DOTFILES with a line more in installers/omp.sh (line 10). */
function dotfilesWith(line: string): Record<string, string> {
  return {
    ...DOTFILES,
    "installers/omp.sh": `${DOTFILES["installers/omp.sh"] ?? ""}${line}\n`,
    "omp/project-agents/reviewer.md":
      "---\nname: alpha-reviewer\ndescription: Reviews, the home's project way.\n---\n",
  };
}

// Catches: a dev box whose installers link a directory as ~/.omp/agents, which Oh My Pi's walk
// from a working directory under the home reads as the project's agents, read as if it held
// none, or read for a session whose working directory is elsewhere.
test("a dev box's ~/.omp/agents its installers link is the project agents of a session under the home with none nearer in the checkout; elsewhere it is not read; an unknown working directory or a line not read as a link is recorded", async () => {
  const root = path.join(dir, "home-agents");
  const linked = dotfilesWith(
    `ensure_link "\${DOTFILES_DIR}/omp/project-agents" "\${HOME}/.omp/agents"`
  );
  const sessionRepository = { repo: "acme/project", commit: COMMIT };
  const where = (cwd: string) => ({
    item: "session repository acme/project",
    source: `The working directory ${cwd} is named project, which acme/project alone of the list is named`,
  });
  const homeAgent: AgentDefinition = {
    name: "alpha-reviewer",
    source: "project",
    repo: "acme/dotfiles",
    commit: COMMIT,
    path: "omp/project-agents/reviewer.md",
    linkedBy: "installers/omp.sh:10",
    autoloadSkills: [],
  };
  const run = (name: string, c: Case, dotfiles = linked, files: Record<string, string> = {}) =>
    projectContext(path.join(root, name), c, dotfiles, { files });
  const repository = { sessionRepository, workingDirectory: "agents/vm" };
  // Under the home, with no .omp/agents in the checkout: the home's is the project's.
  expect(
    await run("home", projectCase("home", repository, [where("/home/alpha/src/project/agents/vm")]))
  ).toEqual({ agent: homeAgent, leftOut: [] });
  // A session no repository holds, under the home, reaches it too.
  expect(
    await run(
      "no-repository",
      projectCase("no-repository", {}, [
        {
          item: "no session repository",
          source:
            "a dev box's working directory is a checkout of a repository the list names. No part of the working directory /home/alpha/notes is named as a repository of the list",
        },
      ])
    )
  ).toEqual({ agent: homeAgent, leftOut: [] });
  // A nearer .omp/agents in the checkout is the one Oh My Pi reads.
  expect(
    (
      await run(
        "nearer",
        projectCase("nearer", repository, [where("/home/alpha/src/project/agents/vm")]),
        linked,
        PROJECT_AGENT_FILES
      )
    ).agent?.path
  ).toBe("agents/vm/.omp/agents/reviewer.md");
  // A working directory outside the home: Oh My Pi's walk does not reach it, and nothing is
  // recorded.
  expect(
    await run("mnt", projectCase("mnt", repository, [where("/mnt/data/project/agents/vm")]))
  ).toEqual({ agent: DOTFILES_REVIEWER, leftOut: [] });
  // No evidence of the working directory: recorded, and not read.
  expect(await run("unknown", projectCase("unknown", repository))).toEqual({
    agent: DOTFILES_REVIEWER,
    leftOut: [
      "the project agents of a dev box's home: acme/dotfiles's installers/omp.sh:10 links omp/project-agents as ~/.omp/agents, which Oh My Pi reads as a project's agents when the session's working directory is under the home, and no evidence item names the working directory, so the context reads none",
    ],
  });
  // A copy into it, which the runner does not read: recorded.
  const copied = dotfilesWith(`cp -r "\${DOTFILES_DIR}/omp/project-agents" "\${HOME}/.omp/agents"`);
  expect(
    await run(
      "copied",
      projectCase("copied", repository, [where("/home/alpha/src/project/agents/vm")]),
      copied
    )
  ).toEqual({
    agent: DOTFILES_REVIEWER,
    leftOut: [
      `the project agents of a dev box's home: acme/dotfiles's installers/omp.sh:10 at ${COMMIT} names ~/.omp/agents in a line the runner does not read as a link of a directory of the repository (ensure_link or ln -s), so the context reads none of it`,
    ],
  });
}, 120_000);

/** `inner` with each read recorded as `<method> <repo>`, and a read of LOCAL_SKILL_REPO refused
 *  as `gh` refuses a repository GitHub does not hold. */
function recordedSource(inner: RepoSource, calls: string[]): RepoSource {
  const read = (method: string, repo: string): void => {
    calls.push(`${method} ${repo}`);
    if (repo === LOCAL_SKILL_REPO) {
      throw new Error(`gh api --method GET repos/${repo}/... failed: gh: Not Found (HTTP 404)`);
    }
  };
  return {
    name: inner.name,
    async file(repo, commit, file) {
      read("file", repo);
      return inner.file(repo, commit, file);
    },
    async tree(repo, commit, dir, dest) {
      read("tree", repo);
      return inner.tree(repo, commit, dir, dest);
    },
    async link(repo, commit, file) {
      read("link", repo);
      return inner.link?.(repo, commit, file);
    },
  };
}

// Catches: a load test's task in a dev box's subagent profile that fails before any run, its
// context reading the installers of the tested skill's own entry, whose repository
// (LOCAL_SKILL_REPO) GitHub does not hold.
test("a load test's own entry of the tested skill names no repository: its context reads nothing of LOCAL_SKILL_REPO and still reads the user agents of the profile's dev-box repository; an arm's tested copy of a real repository is read as one", async () => {
  const root = path.join(dir, "load-own");
  const repos = path.join(root, "repos");
  writeRepositories(repos);
  writeTree(path.join(repos, "acme", "legion", COMMIT), PLUGIN_AGENTS);
  writeTree(path.join(repos, "acme", "dotfiles", COMMIT), DOTFILES);
  const options = (name: string, calls: string[]): ContextOptions => ({
    source: recordedSource(localSource(repos), calls),
    cacheDir: path.join(root, name, "cache"),
    root: path.join(root, name, "scratch"),
  });
  const beta = { repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" };
  // As load.ts's caseOf builds a task's case and arm: the profile's skills and the tested
  // skill's own entry, under LOCAL_SKILL_REPO, at the arm's directory.
  const skillDir = path.join(root, "tested");
  writeTree(skillDir, { "SKILL.md": ALPHA_ARM });
  const own = { repo: LOCAL_SKILL_REPO, commit: "4a02261ae3d020c7", path: skillDir, name: "alpha" };
  const base = aCase("subagent:alpha-reviewer", { plugins: AGENTS_PACK, contextFiles: [] });
  const task: Case = {
    ...base,
    id: "t6",
    profile: { ...base.profile, skills: [beta, GAMMA, own] },
    evidence: [subagentItem("subagent:alpha-reviewer", "devbox")],
  };
  const arm: Arm = {
    name: "old",
    skill: "alpha",
    skillDir,
    installedDir: "/skills/alpha",
    messages: [{ role: "user", content: [{ type: "text", text: "Write the alpha list." }] }],
    profileSkills: task.profile.skills,
  };
  const calls: string[] = [];
  const built = await buildContext(task, arm, options("task", calls));
  expect(calls.filter((call) => call.endsWith(` ${LOCAL_SKILL_REPO}`))).toEqual([]);
  expect(built.record.agent).toEqual(DOTFILES_REVIEWER);
  expect(built.record.skills.find((skill) => skill.name === "alpha")?.from).toBe("arm");
  expect(calls).toContain("tree acme/dotfiles");
  // A behaviour case whose tested copy is the dev-box repository's own: its installers are read.
  const copy = aCase("subagent:alpha-reviewer", {
    plugins: AGENTS_PACK,
    contextFiles: [],
    skills: [{ repo: "acme/dotfiles", commit: COMMIT, path: "skills/alpha", name: "alpha" }, beta],
  });
  const behaviour = { ...copy, evidence: [subagentItem("subagent:alpha-reviewer", "devbox")] };
  writeTree(path.join(root, "old"), { "SKILL.md": ALPHA_ARM });
  const copyArm = buildArm(
    behaviour,
    "old",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    path.join(root, "arm")
  );
  const copied: string[] = [];
  const read = await buildContext(behaviour, copyArm, options("copy", copied));
  expect(read.record.agent).toEqual(DOTFILES_REVIEWER);
  expect(copied).toContain("tree acme/dotfiles");
}, 120_000);
