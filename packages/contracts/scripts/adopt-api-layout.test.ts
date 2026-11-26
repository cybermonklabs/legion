import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

// adopt-api-layout.sh run in scratch repositories that hold the shape of a branch made before main
// stopped committing the generated modules: a base commit that tracks both modules, a main that
// ignores and deletes them, and a branch that changed them, each side also changing one other file
// so a conflict is left for the person. The generator is a stand-in that writes a known render,
// since the script's own work is the version control, not the render.

const script = resolve(import.meta.dir, "adopt-api-layout.sh");
/** The module of each version of the API, as scripts/gen-api.ts renders them. */
const MODULE = "packages/contracts/src/dispatch-api.generated.ts";
const MODULE_V2 = "packages/contracts/src/dispatch-api-v2.generated.ts";
const MODULES = [MODULE, MODULE_V2];
const RENDER = "// rendered by the stand-in\n";
/** The real jj on PATH, and whether this run is on GitHub Actions (`GITHUB_ACTIONS`, not `CI`,
 * which agent harnesses on the devbox export), as pi-shared's jj test reads them. */
const JJ = Bun.which("jj");
const ON_ACTIONS = process.env.GITHUB_ACTIONS === "true";

const ENV = {
  ...process.env,
  GIT_AUTHOR_NAME: "t",
  GIT_AUTHOR_EMAIL: "t@example.invalid",
  GIT_COMMITTER_NAME: "t",
  GIT_COMMITTER_EMAIL: "t@example.invalid",
  JJ_USER: "t",
  JJ_EMAIL: "t@example.invalid",
  JJ_CONFIG: "/dev/null",
};

const roots: string[] = [];
afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { force: true, recursive: true });
});

function run(root: string, cmd: string[]) {
  const done = Bun.spawnSync({ cmd, cwd: root, env: ENV, stdout: "pipe", stderr: "pipe" });
  const stdout = done.stdout.toString();
  return { code: done.exitCode, stdout, out: `${stdout}${done.stderr.toString()}` };
}

/** Runs a command that must succeed and answers what it printed on stdout. */
function must(root: string, cmd: string[]) {
  const done = run(root, cmd);
  if (done.code !== 0) throw new Error(`${cmd.join(" ")} exited ${done.code}: ${done.out}`);
  return done.stdout;
}

function write(root: string, path: string, text: string) {
  mkdirSync(dirname(join(root, path)), { recursive: true });
  writeFileSync(join(root, path), text);
}

/** The scratch repository, on branch `branch`, with `main` beside it. `branchFiles` are written
 * into the branch's commit beside its change to the modules. */
function scratch(branchFiles: Record<string, string> = {}) {
  const root = mkdtempSync(join(tmpdir(), "adopt-api-layout-"));
  roots.push(root);
  must(root, ["git", "init", "-q", "-b", "main", "."]);
  write(root, ".gitignore", "node_modules/\n");
  for (const module of MODULES) write(root, module, "// committed render\n");
  write(root, "notes.txt", "base\n");
  write(
    root,
    "packages/contracts/scripts/gen-api.ts",
    `for (const module of ${JSON.stringify(MODULES)}) await Bun.write(module, ${JSON.stringify(RENDER)});\n`
  );
  must(root, ["git", "add", "-A"]);
  must(root, ["git", "commit", "-qm", "base"]);
  must(root, ["git", "checkout", "-qb", "branch"]);
  for (const module of MODULES) write(root, module, "// the branch's render\n");
  write(root, "notes.txt", "branch\n");
  for (const [path, text] of Object.entries(branchFiles)) write(root, path, text);
  must(root, ["git", "add", "-A"]);
  must(root, ["git", "commit", "-qm", "branch"]);
  must(root, ["git", "checkout", "-q", "main"]);
  write(root, ".gitignore", `node_modules/\n${MODULES.join("\n")}\n`);
  must(root, ["git", "rm", "-q", ...MODULES]);
  write(root, "notes.txt", "main\n");
  must(root, ["git", "commit", "-qam", "main"]);
  must(root, ["git", "checkout", "-q", "branch"]);
  return root;
}

describe("adopt-api-layout.sh", () => {
  it("with git, takes the modules out of the merge, renders them, and lists the other conflict", () => {
    const root = scratch();
    expect(run(root, ["git", "merge", "--no-edit", "main"]).code).not.toBe(0);
    const done = run(root, ["bash", script]);
    expect(done.out).toContain("still in conflict, yours to resolve:\n  notes.txt");
    expect(done.code).toBe(0);
    for (const module of MODULES) {
      expect(must(root, ["git", "ls-files", "--", module])).toBe("");
      expect(readFileSync(join(root, module), "utf8")).toBe(RENDER);
    }
  }, 60_000);

  it.skipIf(JJ === null && !ON_ACTIONS)(
    "with jj, takes the modules out of the merge commit, renders them, and lists the other conflict",
    () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = scratch();
      must(root, ["jj", "git", "init", "--colocate"]);
      must(root, ["jj", "new", "branch", "main", "-m", "merge main"]);
      const done = run(root, ["bash", script]);
      expect(done.out).toContain("still in conflict, yours to resolve:\n  notes.txt");
      expect(done.code).toBe(0);
      for (const module of MODULES) {
        expect(must(root, ["jj", "file", "list", "-r", "@", "--", module])).toBe("");
        expect(readFileSync(join(root, module), "utf8")).toBe(RENDER);
      }
    },
    60_000
  );

  it("refuses before main is merged, and leaves the modules tracked", () => {
    const root = scratch();
    const done = run(root, ["bash", script]);
    expect(done.code).toBe(1);
    expect(done.out).toContain("the root .gitignore does not name");
    for (const module of MODULES) {
      expect(must(root, ["git", "ls-files", "--", module])).toBe(`${module}\n`);
    }
  }, 60_000);

  // The control for step 5: the branch re-includes the /api/v1 module through a nested
  // .gitignore, so jj refuses to untrack it, and the script names that module and fails rather
  // than reporting success; the /api/v2 module, which nothing re-includes, leaves the commit.
  it.skipIf(JJ === null && !ON_ACTIONS)(
    "fails while the merge commit still holds a module",
    () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = scratch({ "packages/contracts/.gitignore": "!src/dispatch-api.generated.ts\n" });
      must(root, ["jj", "git", "init", "--colocate"]);
      must(root, ["jj", "new", "branch", "main", "-m", "merge main"]);
      const done = run(root, ["bash", script]);
      expect(done.code).toBe(1);
      expect(done.out).toContain(`the commit still holds ${MODULE}, which`);
      expect(must(root, ["jj", "file", "list", "-r", "@", "--", MODULE])).toBe(`${MODULE}\n`);
      expect(must(root, ["jj", "file", "list", "-r", "@", "--", MODULE_V2])).toBe("");
    },
    60_000
  );
});
