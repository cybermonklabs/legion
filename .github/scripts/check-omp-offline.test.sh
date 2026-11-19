#!/usr/bin/env bash
# Tests for `check-omp-offline.sh`: a Go test that starts Oh My Pi in a function that never calls
# testbin's offline helper is refused, by its door (testbin.OMP) and by its exec.Command alike, and
# so is a TypeScript start whose environment calls neither ompOffline nor ompEnv; the helper form
# and the reasoned form of each pass; a reason six lines up is too far; a start spelled in a
# comment or a string starts nothing; a regular expression holding a backtick hides no start after
# it; a Sync start under the helper is refused, since the helper's listener cannot answer while the
# call holds the event loop; a helper that stopped naming HTTPS_PROXY fails every start it would
# pass; and a scan that read nothing, or found no start where testbin exists, reports UNKNOWN and
# exits 2.
#
# The binary is followed by its value, whatever its name: a spawn of process.env.LEGION_TEST_OMP
# itself, a name bound to it, Bun.env, a constant naming it, a function of another file that
# returns it, Bun.which("omp"), a joined or a literal path, a shell's -c, Bun's $`...`, execSync
# and exec of a command string, each read as shell; in Go exec.LookPath("omp"), filepath.Join(...,
# "omp"), a literal path, a function of another file of the package that returns it, bash -c, and a
# binary from a helper that calls OMPOffline itself. The helper counts only in the start's own call
# or environment: one in the next statement holds nothing, and an environment the call names, or
# a closure is given at its every call, is followed to it. A start in a file that is no test code
# is not read, and one in a helper under __tests__ is. Last, the real repository passes over a
# count of starts it actually found.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root.
#
# Run from anywhere: .github/scripts/check-omp-offline.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-omp-offline.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# empty <name>: a tree holding the script and nothing else. Echoes its root.
empty() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts"
  ln -s "$check_script" "$root/.github/scripts/check-omp-offline.sh"
  echo "$root"
}

# put <root> <path>: writes the file at <path> under <root> from this harness's stdin.
put() {
  mkdir -p "$(dirname "$1/$2")"
  cat > "$1/$2"
}

# fixture <name>: a tree with both helpers, as testbin and the harness hold them, one Go test that
# starts Oh My Pi under testbin.OMPEnviron (two starts: its door and its exec.Command) and the
# harness's own start under ompEnv (one). Echoes its root.
fixture() {
  local root
  root=$(empty "$1")
  put "$root" packages/daemon/internal/testbin/omp.go <<'EOF'
package testbin

import "testing"

func OMPEnviron(t *testing.T, home string) []string {
	return []string{"HOME=" + home, OMPOffline(t)}
}

func OMPOffline(t *testing.T) string {
	return "HTTPS_PROXY=http://127.0.0.1:1"
}
EOF
  put "$root" packages/daemon/internal/podsafety/helped_test.go <<'EOF'
package podsafety

func TestHelped(t *testing.T) {
	omp := testbin.OMP(t)
	cmd := exec.Command(omp, "models", "list")
	cmd.Env = testbin.OMPEnviron(t, t.TempDir())
}
EOF
  put "$root" packages/pi-shared/test/omp-offline.ts <<'EOF'
export function ompOffline(cleanup: (() => Promise<void>)[]): { readonly HTTPS_PROXY: string } {
  return { HTTPS_PROXY: "http://127.0.0.1:1" };
}
EOF
  put "$root" packages/pi-shared/test/omp-harness.ts <<'EOF'
import { ompOffline } from "./omp-offline";

function ompEnv(home: string, cleanup: (() => Promise<void>)[]): Record<string, string> {
  return { HOME: home, ...ompOffline(cleanup) };
}

export function spawnRpc(binary: string, home: string, cleanup: (() => Promise<void>)[]) {
  return Bun.spawn([binary, "--mode", "rpc"], {
    env: { ...ompEnv(home, cleanup) },
  });
}
EOF
  echo "$root"
}

run_check() { # run_check <root>
  set +e
  out=$("$1/.github/scripts/check-omp-offline.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the helper form of each language passes, every start counted"
root=$(fixture helped)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the two Go starts and the TypeScript one" "$(contains "$out" '3 starts of Oh My Pi in 4 test files read (Go 2, TypeScript 1)')"
check "all three under the helper" "$(contains "$out" 'offline helper (3) or carries a reason (0)')"

echo "case: a Go test that starts Oh My Pi without the helper is refused, door and exec alike"
root=$(fixture go_bare)
put "$root" packages/daemon/internal/podsafety/bare_test.go <<'EOF'
package podsafety

func TestBare(t *testing.T) {
	omp := testbin.OMP(t)
	cmd := exec.Command(omp, "models", "list")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the door" "$(contains "$out" 'bare_test.go:4: a test takes the real Oh My Pi (testbin.OMP)')"
check "names the exec" "$(contains "$out" 'bare_test.go:5: a test starts Oh My Pi')"
check "annotates the line" "$(contains "$out" '::error file=packages/daemon/internal/podsafety/bare_test.go,line=4::')"
check "passes the helped test beside it" "$(is "$(contains "$out" 'helped_test.go')" false)"

echo "case: an exec of a field named omp, in a function apart from the door, is refused"
root=$(fixture go_field)
put "$root" packages/daemon/internal/daemon/live_test.go <<'EOF'
package daemon

func (r *live) paneOmp(args ...string) *exec.Cmd {
	cmd := exec.Command(r.omp, args...)
	cmd.Env = []string{"HOME=" + r.home}
	return cmd
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(contains "$out" 'live_test.go:4: a test starts Oh My Pi')"

echo "case: a read of LEGION_TEST_OMP through a constant is a door"
root=$(fixture go_getenv)
put "$root" packages/daemon/internal/daemon/vars.go <<'EOF'
package daemon

const ompVariable = "LEGION_TEST_OMP"
EOF
put "$root" packages/daemon/internal/daemon/getenv_test.go <<'EOF'
package daemon

func TestGetenv(t *testing.T) {
	binary := os.Getenv(ompVariable)
	run(binary)
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the read" "$(contains "$out" 'getenv_test.go:4: a test reads LEGION_TEST_OMP')"

echo "case: the reasoned form of a Go start passes"
root=$(fixture go_reasoned)
put "$root" packages/daemon/internal/daemon/install_test.go <<'EOF'
package daemon

func installPlugins(t *testing.T, omp string, environ []string) {
	// omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	install := exec.Command(omp, "plugin", "install", "dir")
	install.Env = environ
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the reason" "$(contains "$out" 'offline helper (3) or carries a reason (1)')"

echo "case: a reason six lines up is too far"
root=$(fixture go_distant)
put "$root" packages/daemon/internal/daemon/distant_test.go <<'EOF'
package daemon

func installPlugins(t *testing.T, omp string, environ []string) {
	// omp-offline: environ is the caller's, which testbin.OMPEnviron makes.
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
	install := exec.Command(omp, "plugin", "install", "dir")
	install.Env = environ
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(contains "$out" 'distant_test.go:10:')"

echo "case: a Go start spelled in a comment or a string starts nothing"
root=$(fixture go_prose)
put "$root" packages/daemon/internal/daemon/prose_test.go <<'EOF'
package daemon

// A test once ran omp := testbin.OMP(t) and exec.Command(omp, "models", "list") with no proxy.
func TestProse(t *testing.T) {
	message := `exec.Command(omp, "models") and testbin.OMP(t)`
	other := "exec.Command(omp, \"models\")"
	t.Log(message, other)
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no start of its own" "$(contains "$out" '3 starts of Oh My Pi in 5 test files read')"

echo "case: a TypeScript start whose environment calls no helper is refused"
root=$(fixture ts_bare)
put "$root" packages/pi-envoy/extensions/bare-omp.test.ts <<'EOF'
const omp = process.env.LEGION_TEST_OMP;

test("models", async () => {
  const listed = Bun.spawn([omp!, "models", "list"], {
    env: { HOME: "/tmp/home", PATH: "/usr/bin:/bin" },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the start" "$(contains "$out" 'bare-omp.test.ts:4: a start of the real Oh My Pi whose environment calls neither ompOffline nor ompEnv')"
check "annotates the line" "$(contains "$out" '::error file=packages/pi-envoy/extensions/bare-omp.test.ts,line=4::')"

echo "case: the helper form of a TypeScript start in a test passes"
root=$(fixture ts_helped)
put "$root" packages/pi-envoy/extensions/helped-omp.test.ts <<'EOF'
import { ompOffline } from "@legion/pi-shared/test/omp-offline";

test("models", async () => {
  const listed = Bun.spawn([binary, "models", "refresh"], {
    cwd: workspace,
    env: { HOME: home, PATH: "/usr/bin:/bin", ...ompOffline(cleanup) },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

echo "case: the reasoned form of a TypeScript start passes"
root=$(fixture ts_reasoned)
put "$root" packages/pi-envoy/extensions/tmux-omp.test.ts <<'EOF'
async function tmuxCommand(binary: string, args: readonly string[], environment: Record<string, string>) {
  // omp-offline: `binary` is tmux; its server runs under paneEnvironment's, which holds the proxy.
  const child = Bun.spawn([binary, "-u", ...args], {
    env: environment,
  });
  return child.exited;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the reason" "$(contains "$out" 'offline helper (3) or carries a reason (1)')"

echo "case: a TypeScript start spelled in a comment or a string starts nothing"
root=$(fixture ts_prose)
put "$root" packages/pi-envoy/extensions/prose-omp.test.ts <<'EOF'
// Bun.spawn([omp, "models", "list"]) once ran here with no proxy.
const code = `const r = Bun.spawnSync([omp, "agents"]);`;
const text = 'Bun.spawn([binary, "models"])';
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no start of its own" "$(contains "$out" '3 starts of Oh My Pi in 5 test files read')"

# A backtick inside a regular expression literal, read as the opening of a template, would blank
# every line up to the next backtick and hide whatever start sits there. The second line is the
# one that would: in the first, the quote before the backtick opens a string the line's end closes.
echo "case: a regular expression holding a backtick hides no start after it"
root=$(fixture ts_regex)
put "$root" packages/pi-envoy/extensions/regex-omp.test.ts <<'EOF'
const QUOTED = /["$`\\]/;
const TICKS = /`+/g;

test("models", async () => {
  const listed = Bun.spawn([omp, "models", "list"], {
    env: { HOME: home },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the start" "$(contains "$out" 'regex-omp.test.ts:5: a start of the real Oh My Pi')"

echo "case: a Sync start under the helper is refused"
root=$(fixture ts_sync)
put "$root" packages/pi-shared/test/agents.ts <<'EOF'
import { ompOffline } from "./omp-offline";

export function bundledAgents(omp: string, cleanup: (() => Promise<void>)[]) {
  const unpacked = Bun.spawnSync([omp, "agents", "unpack"], {
    env: { HOME: "/tmp/home", ...ompOffline(cleanup) },
  });
  return unpacked.exitCode;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "says why" "$(contains "$out" 'agents.ts:4: a Sync start of Oh My Pi under ompOffline.s proxy')"

# named <file>:<line>: whether the last run annotated that line.
named() { contains "$out" "^::error file=[^,]*$1,line=$2::"; }

echo "case: a spawn of process.env.LEGION_TEST_OMP itself is a start"
root=$(fixture ts_door_itself)
put "$root" packages/pi-envoy/extensions/door-omp.test.ts <<'EOF'
test("models", async () => {
  const listed = Bun.spawn([process.env.LEGION_TEST_OMP!, "models", "list"], {
    env: { HOME: "/tmp/home" },
  });
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the spawn" "$(named door-omp.test.ts 2)"

echo "case: Bun's \$\`\${omp} ...\` is a start, read as shell, and the .env() chained to it holds the helper"
root=$(fixture ts_dollar)
put "$root" packages/pi-envoy/extensions/dollar-omp.test.ts <<'EOF'
import { $ } from "bun";
const pinned = process.env.LEGION_TEST_OMP as string;

test("bare", async () => {
  await $`${pinned} models list`.env({ HOME: "/tmp/home" });
});
test("after cd", async () => {
  await $`cd ${dir} && ${pinned} models list`;
});
test("helped", async () => {
  await $`${pinned} models list`.env({ HOME: "/tmp/home", ...ompOffline(cleanup) });
});
test("another program", async () => {
  await $`echo ${pinned}`;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare one" "$(named dollar-omp.test.ts 5)"
check "names the one after cd" "$(named dollar-omp.test.ts 8)"
check "passes the helped one" "$(is "$(named dollar-omp.test.ts 11)" false)"
check "counts three starts, and no start of echo" "$(contains "$out" '2 problems over the 6 starts of Oh My Pi')"

echo "case: the binary under any name: LEGION_TEST_OMP, Bun.env, a constant naming it, a function that returns it"
root=$(fixture ts_values)
put "$root" packages/pi-envoy/extensions/pinned.ts <<'EOF'
export const PINNED_VARIABLE = "LEGION_TEST_OMP";

export function pinnedBinary(): string {
  return process.env.LEGION_TEST_OMP ?? "";
}
EOF
put "$root" packages/pi-envoy/extensions/values-omp.test.ts <<'EOF'
import { PINNED_VARIABLE, pinnedBinary } from "./pinned";

const VARIABLE = "LEGION_TEST_OMP";
const renamed = process.env.LEGION_TEST_OMP;
const fromBun = Bun.env.LEGION_TEST_OMP as string;
const byConstant = process.env[VARIABLE]!;

test("each", async () => {
  await Bun.spawn([renamed!, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([fromBun, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([byConstant, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([pinnedBinary(), "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([process.env[PINNED_VARIABLE]!, "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the name bound to it" "$(named values-omp.test.ts 9)"
check "names Bun.env's" "$(named values-omp.test.ts 10)"
check "names the constant's" "$(named values-omp.test.ts 11)"
check "names the function's, from the file it is imported from" "$(named values-omp.test.ts 12)"
check "names the one by another file's constant" "$(named values-omp.test.ts 13)"

echo "case: the binary by which, a joined path, a literal path, an argv or cmd: by name, and in a shell's -c"
root=$(fixture ts_paths)
put "$root" packages/pi-envoy/extensions/paths-omp.test.ts <<'EOF'
import path from "node:path";
const found = Bun.which("omp")!;
const joined = path.join(home, ".local", "bin", "omp");

test("each", async () => {
  await Bun.spawn([found, "models"], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn([joined, "models"], { env: { HOME: "/tmp/home" } }).exited;
  const argv = [found, "models"];
  await Bun.spawn(argv, { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn(["bash", "-c", `${found} models list`], { env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn({ cmd: [found, "models"], env: { HOME: "/tmp/home" } }).exited;
  await Bun.spawn({ cmd: [found, "models"], env: { ...ompOffline(cleanup) } }).exited;
});
EOF
put "$root" packages/pi-envoy/extensions/literal-omp.test.ts <<'EOF'
test("literal", async () => {
  await Bun.spawn(["/usr/local/bin/omp", "models"], { env: { HOME: "/tmp/home" } }).exited;
});
EOF
put "$root" packages/pi-envoy/extensions/shell-string-omp.test.ts <<'EOF'
import { spawn } from "node:child_process";
test("shell", () => {
  spawn("omp models list", { shell: true, env: { HOME: "/tmp/home" } });
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names Bun.which's" "$(named paths-omp.test.ts 6)"
check "names the joined path" "$(named paths-omp.test.ts 7)"
check "names an argv bound to a name" "$(named paths-omp.test.ts 9)"
check "names the shell's -c" "$(named paths-omp.test.ts 10)"
check "names cmd: with an env of no helper" "$(named paths-omp.test.ts 11)"
check "passes cmd: with the helper in its env" "$(is "$(named paths-omp.test.ts 12)" false)"
check "names the literal path, alone in its file" "$(named literal-omp.test.ts 2)"
check "names a command line spawn gives a shell, alone in its file" "$(named shell-string-omp.test.ts 3)"

echo "case: the environment a method is given is followed to its calls"
root=$(fixture ts_method)
put "$root" packages/pi-envoy/extensions/method-omp.test.ts <<'EOF'
const runner = {
  start(env: Record<string, string>) {
    return Bun.spawn([omp, "models"], { env });
  },
};
runner.start({ HOME: "/tmp/home", ...ompOffline(cleanup) });
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

echo "case: execSync and exec of a command string are starts, read as shell; a Sync one under the helper is refused"
root=$(fixture ts_exec)
put "$root" packages/pi-envoy/extensions/exec-omp.test.ts <<'EOF'
import { exec, execSync } from "node:child_process";
const pinned = process.env.LEGION_TEST_OMP as string;

test("each", () => {
  execSync(`${pinned} models list`, { env: { HOME: "/tmp/home" } });
  exec(`${pinned} models list`, { env: { HOME: "/tmp/home" } }, () => {});
  execSync(`${pinned} models list`, { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } });
  exec(pinned + " models list", { env: { HOME: "/tmp/home", ...ompOffline(cleanup) } }, () => {});
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the bare execSync" "$(named exec-omp.test.ts 5)"
check "names the bare exec" "$(named exec-omp.test.ts 6)"
check "refuses the Sync one under the helper" "$(contains "$out" 'exec-omp.test.ts:7: a Sync start of Oh My Pi')"
check "passes exec under the helper" "$(is "$(named exec-omp.test.ts 8)" false)"
check "counts the four" "$(contains "$out" '3 problems over the 7 starts of Oh My Pi')"

echo "case: a helper in the next statement holds nothing"
root=$(fixture ts_next)
put "$root" packages/pi-envoy/extensions/next-omp.test.ts <<'EOF'
test("next", async () => {
  const listed = Bun.spawn([omp, "models", "list"], { env: { HOME: "/tmp/home" } });
  const proxy = ompOffline(cleanup);
  await listed.exited;
});
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the spawn" "$(named next-omp.test.ts 2)"

echo "case: an environment the call names is followed to the helper"
root=$(fixture ts_env_name)
put "$root" packages/pi-envoy/extensions/named-env-omp.test.ts <<'EOF'
test("named", async () => {
  const env = { HOME: "/tmp/home", ...ompOffline(cleanup) };
  const listed = Bun.spawn([omp, "models", "list"], { env });
  await listed.exited;
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it under the helper" "$(contains "$out" 'offline helper (4) or carries a reason (0)')"

echo "case: a start in a file that is no test code is not read, in either language"
root=$(fixture non_test)
put "$root" packages/daemon/internal/podsafety/runner.go <<'EOF'
package podsafety

func Models(home string) error {
	cmd := exec.Command(os.Getenv("LEGION_TEST_OMP"), "models", "list")
	cmd.Env = []string{"HOME=" + home}
	return cmd.Run()
}
EOF
put "$root" packages/pi-envoy/src/run-pinned.ts <<'EOF'
export async function models() {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: "/tmp/home" } }).exited;
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts only the fixture's starts" "$(contains "$out" '3 starts of Oh My Pi in 4 test files read')"

echo "case: a helper under __tests__ is test code"
root=$(fixture tests_helper)
put "$root" packages/pi-envoy/src/__tests__/omp-helper.ts <<'EOF'
export async function models(home: string) {
  await Bun.spawn([process.env.LEGION_TEST_OMP!, "models"], { env: { HOME: home } }).exited;
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names its start" "$(named omp-helper.ts 2)"

echo "case: a Go exec of the binary under any name: LookPath, a joined path, a literal path, a function or method that returns it, env, bash -c"
root=$(fixture go_values)
put "$root" packages/daemon/internal/podsafety/pinned.go <<'EOF'
package podsafety

// pinnedBinary is the binary LEGION_TEST_OMP names.
func pinnedBinary() string {
	return os.Getenv("LEGION_TEST_OMP")
}

type rig struct{}

func (rig) binary() string {
	return os.Getenv("LEGION_OMP_PATH")
}
EOF
put "$root" packages/daemon/internal/podsafety/values_test.go <<'EOF'
package podsafety

func TestValues(t *testing.T) {
	found, err := exec.LookPath("omp")
	if err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	byPath := exec.Command(found, "models", "list")
	byPath.Env = []string{"HOME=" + home}
	joined := exec.Command(filepath.Join(home, ".local", "bin", "omp"), "models")
	joined.Env = []string{"HOME=" + home}
	returned := exec.Command(pinnedBinary(), "models")
	returned.Env = []string{"HOME=" + home}
	var r rig
	method := exec.Command(r.binary(), "models")
	method.Env = []string{"HOME=" + home}
	wrapped := exec.Command("env", "HOME="+home, found, "models")
	wrapped.Env = []string{"HOME=" + home}
	run := func(program string) *exec.Cmd {
		return exec.Command(program, "models")
	}
	_ = run(found)
}
EOF
put "$root" packages/daemon/internal/podsafety/literal_test.go <<'EOF'
package podsafety

func TestLiteral(t *testing.T) {
	home := t.TempDir()
	literal := exec.Command("/usr/local/bin/omp", "models")
	literal.Env = []string{"HOME=" + home}
	shell := exec.Command("bash", "-c", "omp models list")
	shell.Env = []string{"HOME=" + home}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names LookPath as a door" "$(contains "$out" 'values_test.go:4: a test resolves the real Oh My Pi (exec.LookPath)')"
check "names the exec of LookPath's" "$(named values_test.go 9)"
check "names the joined path" "$(named values_test.go 11)"
check "names the function's, from another file of the package" "$(named values_test.go 13)"
check "names the method's, which reads LEGION_OMP_PATH" "$(named values_test.go 16)"
check "names it after env" "$(named values_test.go 18)"
check "names it as a parameter the closure's call is given" "$(named values_test.go 21)"
check "names the literal path, in a file with no door" "$(named literal_test.go 5)"
check "names bash -c, in a file with no door" "$(named literal_test.go 7)"
check "does not read the non-test file's Getenv as a door" "$(is "$(contains "$out" 'pinned.go')" false)"

echo "case: a Go exec's own Cmd's environment decides, not a helper elsewhere in its function"
root=$(fixture go_own_env)
put "$root" packages/daemon/internal/podsafety/own_test.go <<'EOF'
package podsafety

func TestOwn(t *testing.T) {
	omp := testbin.OMP(t)
	offline := testbin.OMPOffline(t)
	held := exec.Command(omp, "models", "list")
	held.Env = []string{"HOME=" + t.TempDir(), offline}
	offline = "PATH=/usr/bin"
	bare := exec.Command(omp, "config", "get", "x")
	bare.Env = []string{"HOME=" + t.TempDir()}
	_ = testbin.OMPOffline(t)
	extended := exec.Command(omp, "models")
	extended.Env = testbin.OMPEnviron(t, t.TempDir())
	extended.Env = append(extended.Env, "X=1")
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec whose Env holds no helper" "$(named own_test.go 9)"
check "passes the exec whose Env names the helper's entry, set again only after it" "$(is "$(named own_test.go 6)" false)"
check "passes the exec whose Env appends to the helper's" "$(is "$(named own_test.go 12)" false)"
check "passes the door, in a function that calls the helper" "$(is "$(named own_test.go 4)" false)"
check "and nothing else" "$(contains "$out" '^1 problems over')"

echo "case: a binary from a helper that calls OMPOffline is held to its own exec's environment"
root=$(fixture go_split)
put "$root" packages/daemon/internal/podsafety/split_test.go <<'EOF'
package podsafety

func pinned(t *testing.T) string {
	_ = testbin.OMPOffline(t)
	return testbin.OMP(t)
}

func TestSplit(t *testing.T) {
	bin := pinned(t)
	cmd := exec.Command(bin, "models", "list")
	cmd.Env = []string{"HOME=" + t.TempDir()}
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the exec" "$(named split_test.go 10)"
check "passes the door, in the helper that calls OMPOffline" "$(is "$(named split_test.go 5)" false)"

echo "case: the environment a closure is given is followed to its every call"
root=$(fixture go_param)
put "$root" packages/daemon/internal/podsafety/param_test.go <<'EOF'
package podsafety

func TestParam(t *testing.T) {
	omp := testbin.OMP(t)
	bare := []string{"HOME=" + t.TempDir(), testbin.OMPOffline(t)}
	run := func(env []string) {
		cmd := exec.Command(omp, "models", "list")
		cmd.Env = env
	}
	run(bare)
	run(append(bare, "X=1"))
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the door and the exec under the helper" "$(contains "$out" 'offline helper (5) or carries a reason (0)')"
root=$(fixture go_param_bare)
put "$root" packages/daemon/internal/podsafety/param_test.go <<'EOF'
package podsafety

func TestParam(t *testing.T) {
	omp := testbin.OMP(t)
	bare := []string{"HOME=" + t.TempDir(), testbin.OMPOffline(t)}
	run := func(env []string) {
		cmd := exec.Command(omp, "models", "list")
		cmd.Env = env
	}
	run(bare)
	run([]string{"HOME=" + t.TempDir()})
}
EOF
run_check "$root"
check "fails when one call gives it no helper" "$(is "$status" 1)"
check "names the exec" "$(named param_test.go 7)"

echo "case: a helper that stopped naming HTTPS_PROXY fails"
root=$(fixture helper_bare)
put "$root" packages/daemon/internal/testbin/omp.go <<'EOF'
package testbin

import "testing"

func OMPEnviron(t *testing.T, home string) []string {
	return []string{"HOME=" + home, OMPOffline(t)}
}

func OMPOffline(t *testing.T) string {
	return "PATH=/usr/bin"
}
EOF
put "$root" packages/pi-shared/test/omp-harness.ts <<'EOF'
function ompEnv(home: string): Record<string, string> {
  return { HOME: home };
}

export function spawnRpc(binary: string, home: string) {
  return Bun.spawn([binary, "--mode", "rpc"], {
    env: { ...ompEnv(home) },
  });
}
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the Go helper" "$(contains "$out" 'omp.go:9: no Go OMPOffline that names HTTPS_PROXY')"
check "names the environment helper that calls no ompOffline" "$(contains "$out" 'omp-harness.ts:1: ompEnv does not call ompOffline')"

echo "case: an empty tree is UNKNOWN"
root=$(empty nothing)
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says what it could not read" "$(contains "$out" 'UNKNOWN: read no Go and no TypeScript test file')"

echo "case: testbin with no start of Oh My Pi anywhere is UNKNOWN"
root=$(fixture no_start)
rm "$root/packages/daemon/internal/podsafety/helped_test.go"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says the reading sees none" "$(contains "$out" 'UNKNOWN: packages/daemon/internal/testbin exists and no Go test starts Oh My Pi')"

# The count matters as much as the exit status here: a script that resolved the wrong root would
# scan nothing and could not print this line, and one whose patterns went blind would print a
# count of zero.
echo "case: the real repository passes, over starts it actually found"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "starts of each language counted" "$(contains "$out" '(Go [1-9][0-9]*, TypeScript [1-9][0-9]*)')"

summary "check-omp-offline.sh"
