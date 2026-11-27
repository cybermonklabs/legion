import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";

// adopt-api-layout.sh run in scratch repositories that hold the shape of a branch made before main
// stopped committing the generated module: a base commit that tracks the module, a main that
// ignores and deletes it, and a branch that changed it, each side also changing one other file so
// a conflict is left for the person. The generator is a stand-in that writes a known render, since
// the script's own work is the version control, not the render.

const script = resolve(import.meta.dir, "adopt-api-layout.sh");
const MODULE = "packages/contracts/src/dispatch-api.generated.ts";
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
 * into the branch's commit beside its change to the module. */
function scratch(branchFiles: Record<string, string> = {}) {
  const root = mkdtempSync(join(tmpdir(), "adopt-api-layout-"));
  roots.push(root);
  must(root, ["git", "init", "-q", "-b", "main", "."]);
  write(root, ".gitignore", "node_modules/\n");
  write(root, MODULE, "// committed render\n");
  write(root, "notes.txt", "base\n");
  write(
    root,
    "packages/contracts/scripts/gen-api.ts",
    `await Bun.write(${JSON.stringify(MODULE)}, ${JSON.stringify(RENDER)});\n`
  );
  must(root, ["git", "add", "-A"]);
  must(root, ["git", "commit", "-qm", "base"]);
  must(root, ["git", "checkout", "-qb", "branch"]);
  write(root, MODULE, "// the branch's render\n");
  write(root, "notes.txt", "branch\n");
  for (const [path, text] of Object.entries(branchFiles)) write(root, path, text);
  must(root, ["git", "add", "-A"]);
  must(root, ["git", "commit", "-qm", "branch"]);
  must(root, ["git", "checkout", "-q", "main"]);
  write(root, ".gitignore", `node_modules/\n${MODULE}\n`);
  must(root, ["git", "rm", "-q", MODULE]);
  write(root, "notes.txt", "main\n");
  must(root, ["git", "commit", "-qam", "main"]);
  must(root, ["git", "checkout", "-q", "branch"]);
  return root;
}

describe("adopt-api-layout.sh", () => {
  it("with git, takes the module out of the merge, renders it, and lists the other conflict", () => {
    const root = scratch();
    expect(run(root, ["git", "merge", "--no-edit", "main"]).code).not.toBe(0);
    const done = run(root, ["bash", script]);
    expect(done.out).toContain("still in conflict, yours to resolve:\n  notes.txt");
    expect(done.code).toBe(0);
    expect(must(root, ["git", "ls-files", "--", MODULE])).toBe("");
    expect(readFileSync(join(root, MODULE), "utf8")).toBe(RENDER);
  }, 60_000);

  it.skipIf(JJ === null && !ON_ACTIONS)(
    "with jj, takes the module out of the merge commit, renders it, and lists the other conflict",
    () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = scratch();
      must(root, ["jj", "git", "init", "--colocate"]);
      must(root, ["jj", "new", "branch", "main", "-m", "merge main"]);
      const done = run(root, ["bash", script]);
      expect(done.out).toContain("still in conflict, yours to resolve:\n  notes.txt");
      expect(done.code).toBe(0);
      expect(must(root, ["jj", "file", "list", "-r", "@", "--", MODULE])).toBe("");
      expect(readFileSync(join(root, MODULE), "utf8")).toBe(RENDER);
    },
    60_000
  );

  it("refuses before main is merged, and leaves the module tracked", () => {
    const root = scratch();
    const done = run(root, ["bash", script]);
    expect(done.code).toBe(1);
    expect(done.out).toContain("the root .gitignore does not name");
    expect(must(root, ["git", "ls-files", "--", MODULE])).toBe(`${MODULE}\n`);
  }, 60_000);

  // The control for step 5: the branch re-includes the module through a nested .gitignore, so jj
  // refuses to untrack it, and the script names the module and fails rather than reporting success.
  it.skipIf(JJ === null && !ON_ACTIONS)(
    "fails while the merge commit still holds the module",
    () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = scratch({ "packages/contracts/.gitignore": "!src/dispatch-api.generated.ts\n" });
      must(root, ["jj", "git", "init", "--colocate"]);
      must(root, ["jj", "new", "branch", "main", "-m", "merge main"]);
      const done = run(root, ["bash", script]);
      expect(done.code).toBe(1);
      expect(done.out).toContain(`the commit still holds ${MODULE}`);
      expect(must(root, ["jj", "file", "list", "-r", "@", "--", MODULE])).toBe(`${MODULE}\n`);
    },
    60_000
  );
});
