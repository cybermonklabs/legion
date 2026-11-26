import { expect, test } from "bun:test";
import { chmodSync, copyFileSync, existsSync, mkdirSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import { requireHostTools } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { versionControlEnvironment } from "@legion/test-fixtures/version-control-environment";

// label-texts.sh is the one entry the rig calls in a label's checkout. Each row copies it into a
// label of its own, a directory laid out as a checkout, so the row decides what that label holds.
const entry = path.join(import.meta.dir, "label-texts.sh");
const scripts = path.join("packages", "pi-legion", "scripts", "skill-scenarios");
const tests = [
  ["workflow", "rig_world_test.go", "TestRigWorld"],
  ["daemon", "rig_launch_test.go", "TestRigLaunch"],
  ["api", "rig_plan_test.go", "TestRigPlanCheck"],
] as const;

const directory = temporaryDirectories("label-texts");

type Environment = Record<string, string | undefined>;

/** A label holding label-texts.sh and nothing else, and its own home for git. */
function bareLabel(): { label: string; script: string; env: Environment } {
  const label = directory("label");
  mkdirSync(path.join(label, scripts), { recursive: true });
  const script = path.join(label, scripts, "label-texts.sh");
  copyFileSync(entry, script);
  const env = { ...process.env, ...versionControlEnvironment(directory("home")) };
  return { label, script, env };
}

function run(cwd: string, env: Environment, ...argv: string[]) {
  const ran = Bun.spawnSync(argv, { cwd, env, stdout: "pipe", stderr: "pipe" });
  return { status: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

// RIG_TEXTS_BIN is set, outside the label, so require_tests alone can refuse the label: with it
// unset, require_bin's own exit 2 would pass the row whatever require_tests does.
test("build in a label without the three test files exits 2, naming each missing test", () => {
  requireHostTools("bash");
  const { label, script, env } = bareLabel();

  const built = run(label, { ...env, RIG_TEXTS_BIN: directory("bin") }, "bash", script, "build");

  expect(built.status).toBe(2);
  for (const [pkg, file, name] of tests) {
    expect(built.stderr).toContain(`packages/daemon/internal/${pkg}/${file}`);
    expect(built.stderr).toContain(name);
  }
});

// Catches: a label whose daemon moved the engine's package, as a refactor would, built as if the
// entry's test were still there. The label holds this checkout's own three test files at their
// own paths; with them all, the build gets past the tests' check to the compile (the planted go
// fails it, exit 1); with internal/workflow moved, it stops at the check, exit 2, naming the file
// and its test. A refactor of this checkout that moves a test file fails the copy below by name.
test("a label whose engine's package moved is refused by name, and one with this checkout's three tests is not", () => {
  requireHostTools("bash", "git");
  const { label, script, env } = bareLabel();
  const root = path.resolve(import.meta.dir, "../../../..");
  for (const [pkg, file] of tests) {
    const from = path.join(root, "packages", "daemon", "internal", pkg, file);
    const dir = path.join(label, "packages", "daemon", "internal", pkg);
    mkdirSync(dir, { recursive: true });
    copyFileSync(from, path.join(dir, file));
  }
  const identity = ["-c", "user.name=Label Test", "-c", "user.email=label@example.invalid"];
  for (const args of [
    ["-c", "init.defaultBranch=main", "init", "-q"],
    ["add", "-A"],
    ["commit", "-qm", "label"],
  ]) {
    const ran = run(label, env, "git", ...identity, ...args);
    if (ran.status !== 0) throw new Error(`git ${args.join(" ")}: ${ran.status} ${ran.stderr}`);
  }
  const tools = directory("go-fails");
  writeFileSync(path.join(tools, "go"), "#!/bin/sh\necho 'go: planted failure' >&2\nexit 1\n", {
    mode: 0o755,
  });
  const withGo = { ...env, PATH: `${tools}:${env.PATH ?? ""}`, RIG_TEXTS_BIN: directory("bin") };

  const whole = run(label, withGo, "bash", script, "build");
  expect({ status: whole.status, stderr: whole.stderr }).toEqual({
    status: 1,
    stderr: expect.stringContaining("go test -c of the three packages failed"),
  });

  const internal = path.join(label, "packages", "daemon", "internal");
  renameSync(path.join(internal, "workflow"), path.join(internal, "engine"));
  const moved = run(label, withGo, "bash", script, "build");
  expect(moved.status).toBe(2);
  expect(moved.stderr).toContain("packages/daemon/internal/workflow/rig_world_test.go");
  expect(moved.stderr).toContain("TestRigWorld");
});

// A test binary exits 0 when -test.run selects nothing, printing a warning and PASS. The label
// below holds the three test files, and its built world binary is such a binary: a planted
// executable that prints what a Go test binary prints when it selects no test, so the row needs
// no Go toolchain and controls exactly that output.
test("world refuses a binary that ran no test, naming the missing PASS line", () => {
  requireHostTools("bash", "git");
  const { label, script, env } = bareLabel();
  for (const [pkg, file] of tests) {
    const dir = path.join(label, "packages", "daemon", "internal", pkg);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, file), `package ${pkg}\n`);
  }
  const identity = ["-c", "user.name=Label Test", "-c", "user.email=label@example.invalid"];
  const git = (...args: string[]) => {
    const ran = run(label, env, "git", ...identity, ...args);
    if (ran.status !== 0) throw new Error(`git ${args.join(" ")}: ${ran.status} ${ran.stderr}`);
    return ran.stdout.trim();
  };
  git("-c", "init.defaultBranch=main", "init", "-q");
  git("add", "-A");
  git("commit", "-qm", "label");
  const texts = directory("texts");
  const bin = path.join(texts, git("rev-parse", "HEAD"));
  mkdirSync(bin, { recursive: true });
  const binary = path.join(bin, "workflow.test");
  writeFileSync(binary, "#!/bin/sh\necho 'testing: warning: no tests to run'\necho PASS\nexit 0\n");
  chmodSync(binary, 0o755);
  const work = directory("run");
  const request = path.join(work, "world-request.json");
  const answer = path.join(work, "world-answer.json");
  writeFileSync(request, "{}\n");

  const world = run(
    label,
    { ...env, RIG_TEXTS_BIN: texts },
    "bash",
    script,
    "world",
    request,
    answer
  );

  expect(world.stderr).toContain("testing: warning: no tests to run");
  expect(world.stderr).toContain("--- PASS: TestRigWorld");
  expect(world.status).toBe(1);
});

// A go that answers the entry's one compile, `go test -c -o <dir>/ <packages>`, with a stand-in
// binary for each package, named as go names them, that prints its test's PASS line.
const GO = `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; fi
  shift
done
for pair in workflow:TestRigWorld daemon:TestRigLaunch api:TestRigPlanCheck; do
  printf '#!/bin/sh\\necho "--- PASS: %s (0.00s)"\\n' "\${pair#*:}" >"\${out}\${pair%%:*}.test"
  chmod +x "\${out}\${pair%%:*}.test"
done
`;

// Catches: a build that writes its binaries into the label's checkout, where git status lists them
// and jj snapshots them into the developer's working-copy commit, and one that takes a directory
// inside the checkout or none at all.
test("build compiles into RIG_TEXTS_BIN and leaves the label's checkout as it was, and refuses a RIG_TEXTS_BIN inside it or unset", () => {
  requireHostTools("bash", "git");
  const { label, script, env } = bareLabel();
  for (const [pkg, file] of tests) {
    const dir = path.join(label, "packages", "daemon", "internal", pkg);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, file), `package ${pkg}\n`);
  }
  const identity = ["-c", "user.name=Label Test", "-c", "user.email=label@example.invalid"];
  const git = (...args: string[]) => {
    const ran = run(label, env, "git", ...identity, ...args);
    if (ran.status !== 0) throw new Error(`git ${args.join(" ")}: ${ran.status} ${ran.stderr}`);
    return ran.stdout.trim();
  };
  git("-c", "init.defaultBranch=main", "init", "-q");
  git("add", "-A");
  git("commit", "-qm", "label");
  const tools = directory("go");
  writeFileSync(path.join(tools, "go"), GO, { mode: 0o755 });
  const withGo = { ...env, PATH: `${tools}:${env.PATH ?? ""}` };
  const out = directory("texts");

  const built = run(label, { ...withGo, RIG_TEXTS_BIN: out }, "bash", script, "build");

  expect({ status: built.status, stderr: built.status === 0 ? "" : built.stderr }).toEqual({
    status: 0,
    stderr: "",
  });
  for (const [, , name] of tests) expect(built.stdout).toContain(`--- PASS: ${name} (`);
  expect(git("status", "--porcelain", "--untracked-files=all", "--ignored")).toBe("");
  expect(existsSync(path.join(out, git("rev-parse", "HEAD"), "built"))).toBe(true);

  const insideDir = path.join(label, "texts");
  const inside = run(label, { ...withGo, RIG_TEXTS_BIN: insideDir }, "bash", script, "build");
  expect(inside.status).toBe(2);
  expect(inside.stderr).toContain(`RIG_TEXTS_BIN ${insideDir} is inside the label's checkout`);
  const unset = run(label, withGo, "bash", script, "build");
  expect(unset.status).toBe(2);
  expect(unset.stderr).toContain("RIG_TEXTS_BIN is unset");
  expect(existsSync(insideDir)).toBe(false);
});
