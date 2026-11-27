import { expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  renameSync,
  writeFileSync,
} from "node:fs";
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
  ["daemon", "rig_daemon_test.go", "TestRigDaemon"],
  ["daemon", "rig_daemon_live_test.go", "TestRigDaemonLive"],
] as const;
// The tests whose own cases build runs: the three that read a request. TestRigDaemonLive has none,
// and TestRigDaemon is a check of its own (CI's daemon-go job), never a step of a label's build.
const ownCases = tests.filter(([, , name]) => name !== "TestRigDaemonLive" && name !== "TestRigDaemon");

const directory = temporaryDirectories("label-texts");

type Environment = Record<string, string | undefined>;
type Test = (typeof tests)[number];

/** A label holding label-texts.sh and nothing else, and its own home for git. */
function bareLabel(): { label: string; script: string; env: Environment } {
  const label = directory("label");
  mkdirSync(path.join(label, scripts), { recursive: true });
  const script = path.join(label, scripts, "label-texts.sh");
  copyFileSync(entry, script);
  const env = { ...process.env, ...versionControlEnvironment(directory("home")) };
  return { label, script, env };
}

/** A bare label that also holds the test files of held, committed as its one commit. */
function committedLabel(held: readonly Test[] = tests) {
  const { label, script, env } = bareLabel();
  for (const [pkg, file] of held) {
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
  return { label, script, env, git, commit: git("rev-parse", "HEAD") };
}

function run(cwd: string, env: Environment, ...argv: string[]) {
  const ran = Bun.spawnSync(argv, { cwd, env, stdout: "pipe", stderr: "pipe" });
  return { status: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

/** Writes text as the executable file at file. */
function plant(file: string, text: string) {
  writeFileSync(file, text);
  chmodSync(file, 0o755);
}

// RIG_TEXTS_BIN is set, outside the label, so require_tests alone can refuse the label: with it
// unset, require_bin's own exit 2 would pass the row whatever require_tests does.
test("build in a label without the five test files exits 2, naming each missing test", () => {
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
// entry's test were still there. The label holds this checkout's own five test files at their
// own paths; with them all, the build gets past the tests' check to the compile (the planted go
// fails it, exit 1); with internal/workflow moved, it stops at the check, exit 2, naming the file
// and its test. A refactor of this checkout that moves a test file fails the copy below by name.
test("a label whose engine's package moved is refused by name, and one with this checkout's five tests is not", () => {
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

test("a label that lacks rig_daemon_test.go exits 2 naming it", () => {
  requireHostTools("bash", "git");
  const { label, script, env } = committedLabel(
    tests.filter(([, file]) => file !== "rig_daemon_test.go")
  );
  const withBin = { ...env, RIG_TEXTS_BIN: directory("texts") };

  for (const argv of [["build"], ["build", "--compile"]]) {
    const built = run(label, withBin, "bash", script, ...argv);
    expect({ argv, status: built.status }).toEqual({ argv, status: 2 });
    expect(built.stderr).toContain("packages/daemon/internal/daemon/rig_daemon_test.go");
    expect(built.stderr).toContain("TestRigDaemon");
    expect(built.stderr).not.toContain("rig_launch_test.go");
  }
});

// A test binary exits 0 when -test.run selects nothing, printing a warning and PASS. The label
// below holds the five test files, and its built world binary is such a binary: a planted
// executable that prints what a Go test binary prints when it selects no test, so the row needs
// no Go toolchain and controls exactly that output.
test("world refuses a binary that ran no test, naming the missing PASS line", () => {
  requireHostTools("bash", "git");
  const { label, script, env, commit } = committedLabel();
  const texts = directory("texts");
  const bin = path.join(texts, commit);
  mkdirSync(bin, { recursive: true });
  plant(
    path.join(bin, "workflow.test"),
    "#!/bin/sh\necho 'testing: warning: no tests to run'\necho PASS\nexit 0\n"
  );
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

// A go that answers the entry's one compile, `go test -c ... -o <dir>/ <packages>`, with a
// stand-in binary for each package, named as go names them, that prints the PASS line of the test
// its -test.run names. The go writes its own argv to GO_ARGV_LOG, and each binary a line to
// RAN_LOG when it runs, where those are set.
const GO = `#!/bin/sh
[ -z "$GO_ARGV_LOG" ] || printf '%s\\n' "$*" >>"$GO_ARGV_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then out=$2; fi
  shift
done
for pkg in workflow daemon api; do
  cat >"\${out}\${pkg}.test" <<'EOF'
#!/bin/sh
[ -z "$RAN_LOG" ] || echo ran >>"$RAN_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -test.run ]; then name=$2; fi
  shift
done
name=\${name#^}
name=\${name%\\$}
echo "--- PASS: $name (0.00s)"
EOF
  chmod +x "\${out}\${pkg}.test"
done
`;

/** The environment of a label whose PATH starts with the planted go (GO). */
function withPlantedGo(env: Environment): Environment {
  const tools = directory("go");
  writeFileSync(path.join(tools, "go"), GO, { mode: 0o755 });
  return { ...env, PATH: `${tools}:${env.PATH ?? ""}` };
}

// Catches: a build that writes its binaries into the label's checkout, where git status lists them
// and jj snapshots them into the developer's working-copy commit, and one that takes a directory
// inside the checkout or none at all.
test("build compiles into RIG_TEXTS_BIN and leaves the label's checkout as it was, and refuses a RIG_TEXTS_BIN inside it or unset", () => {
  requireHostTools("bash", "git");
  const { label, script, env, git, commit } = committedLabel();
  const withGo = withPlantedGo(env);
  const out = directory("texts");

  const built = run(label, { ...withGo, RIG_TEXTS_BIN: out }, "bash", script, "build");

  expect({ status: built.status, stderr: built.status === 0 ? "" : built.stderr }).toEqual({
    status: 0,
    stderr: "",
  });
  for (const [, , name] of ownCases) expect(built.stdout).toContain(`--- PASS: ${name} (`);
  expect(built.stdout).not.toContain("TestRigDaemonLive");
  // Catches: a build that runs the daemon rig, whose shared NATS or missing route then makes a label
  // that compiles read red.
  expect(built.stdout).not.toContain("--- PASS: TestRigDaemon (");
  expect(git("status", "--porcelain", "--untracked-files=all", "--ignored")).toBe("");
  expect(existsSync(path.join(out, commit, "built"))).toBe(true);

  const insideDir = path.join(label, "texts");
  const inside = run(label, { ...withGo, RIG_TEXTS_BIN: insideDir }, "bash", script, "build");
  expect(inside.status).toBe(2);
  expect(inside.stderr).toContain(`RIG_TEXTS_BIN ${insideDir} is inside the label's checkout`);
  const unset = run(label, withGo, "bash", script, "build");
  expect(unset.status).toBe(2);
  expect(unset.stderr).toContain("RIG_TEXTS_BIN is unset");
  expect(existsSync(insideDir)).toBe(false);
});

// Catches: a --compile that runs a test (the route case, which fails on a label without the
// route, would stop a path check before its pane), and a compile that leaves the daemon binary
// without its e2e-tagged test.
test("build --compile makes the binaries and runs no test", () => {
  requireHostTools("bash", "git");
  const { label, script, env, commit } = committedLabel();
  const out = directory("texts");
  const logs = directory("logs");
  const argvLog = path.join(logs, "go-argv.log");
  const ranLog = path.join(logs, "ran.log");

  const built = run(
    label,
    { ...withPlantedGo(env), RIG_TEXTS_BIN: out, GO_ARGV_LOG: argvLog, RAN_LOG: ranLog },
    "bash",
    script,
    "build",
    "--compile"
  );

  expect({ status: built.status, stderr: built.status === 0 ? "" : built.stderr }).toEqual({
    status: 0,
    stderr: "",
  });
  for (const pkg of ["workflow", "daemon", "api"]) {
    expect(existsSync(path.join(out, commit, `${pkg}.test`))).toBe(true);
  }
  expect(existsSync(path.join(out, commit, "built"))).toBe(true);
  expect(existsSync(ranLog)).toBe(false);
  expect(built.stdout).not.toContain("--- PASS");
  const argv = readFileSync(argvLog, "utf8").trim().split(" ");
  expect(argv.slice(0, 4)).toEqual(["test", "-c", "-tags", "e2e"]);
  expect(argv.slice(-3)).toEqual(["./internal/workflow", "./internal/daemon", "./internal/api"]);
});

/** A label whose built daemon binary is the planted text daemon, and the four files of its run. */
function daemonRun(daemon: string) {
  const { label, script, env, commit } = committedLabel();
  const texts = directory("texts");
  const bin = path.join(texts, commit);
  mkdirSync(bin, { recursive: true });
  plant(path.join(bin, "daemon.test"), daemon);
  const work = directory("run");
  const files = {
    request: path.join(work, "request.json"),
    ready: path.join(work, "ready.json"),
    status: path.join(work, "status.json"),
    record: path.join(work, "record.json"),
  };
  writeFileSync(
    files.request,
    `${JSON.stringify({ postgresUrl: "postgres://rig@127.0.0.1:5432/postgres" })}\n`
  );
  const verb = () =>
    run(
      label,
      { ...env, RIG_TEXTS_BIN: texts },
      "bash",
      script,
      "daemon",
      files.request,
      files.ready,
      files.status,
      files.record
    );
  return { files, verb };
}

// The planted daemon binary of a whole run: it writes the three files, each holding what the
// entry handed it (the database server, its argv, the request's path), and prints its PASS line.
const WHOLE_RUN = `#!/bin/sh
printf '%s\\n' "$LEGION_TEST_PG_DSN" >"$RIG_TEXTS_READY"
printf '%s\\n' "$*" >"$RIG_TEXTS_STATUS"
printf '%s\\n' "$RIG_TEXTS_REQUEST" >"$RIG_TEXTS_RECORD"
echo "--- PASS: TestRigDaemonLive (0.01s)"
`;

test("the daemon verb needs the PASS line and its three files", () => {
  requireHostTools("bash", "git", "jq");

  const whole = daemonRun(WHOLE_RUN);
  const passed = whole.verb();
  expect({ status: passed.status, stderr: passed.status === 0 ? "" : passed.stderr }).toEqual({
    status: 0,
    stderr: "",
  });
  expect(passed.stdout).toContain("--- PASS: TestRigDaemonLive (");
  expect(readFileSync(whole.files.ready, "utf8")).toBe("postgres://rig@127.0.0.1:5432/postgres\n");
  expect(readFileSync(whole.files.status, "utf8")).toBe("-test.run ^TestRigDaemonLive$ -test.v\n");
  expect(readFileSync(whole.files.record, "utf8")).toBe(`${realpathSync(whole.files.request)}\n`);

  const noRecord = daemonRun(WHOLE_RUN.replace(/^.*RIG_TEXTS_RECORD.*\n/m, ""));
  const unrecorded = noRecord.verb();
  expect(unrecorded.status).toBe(1);
  expect(unrecorded.stderr).toContain(
    `wrote no record file ${realpathSync(path.dirname(noRecord.files.record))}/record.json`
  );

  const noPass = daemonRun(
    WHOLE_RUN.replace(/^echo .*\n/m, "echo 'testing: warning: no tests to run'\necho PASS\n")
  );
  const unpassed = noPass.verb();
  expect(unpassed.status).toBe(1);
  expect(unpassed.stderr).toContain("printed no '--- PASS: TestRigDaemonLive' line");
});

// The planted daemon binary refuses each request key it does not handle, naming it, as
// TestRigDaemonLive does; the row shows the entry hands the binary the request's own path and
// passes the refusal on.
test("a request with an unknown field fails naming the field", () => {
  requireHostTools("bash", "git", "jq");
  const refusing = daemonRun(`#!/bin/sh
extra=$(jq -r 'keys - ["postgresUrl"] | first // empty' "$RIG_TEXTS_REQUEST")
if [ -n "$extra" ]; then
  echo "the daemon request $RIG_TEXTS_REQUEST: json: unknown field \\"$extra\\""
  echo "--- FAIL: TestRigDaemonLive (0.00s)"
  exit 1
fi
${WHOLE_RUN.split("\n").slice(1).join("\n")}`);
  writeFileSync(
    refusing.files.request,
    `${JSON.stringify({ postgresUrl: "postgres://rig@127.0.0.1:5432/postgres", planGuidanceFile: "/rig/guidance.md" })}\n`
  );

  const refused = refusing.verb();

  expect(refused.status).toBe(1);
  expect(refused.stderr).toContain(
    `the daemon request ${realpathSync(refusing.files.request)}: json: unknown field "planGuidanceFile"`
  );
  expect(existsSync(refusing.files.ready)).toBe(false);
});
