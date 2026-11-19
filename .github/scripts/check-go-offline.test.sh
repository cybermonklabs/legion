#!/usr/bin/env bash
# Tests for `check-go-offline.sh`, over trees it writes. Its first rule: a Go, a TypeScript, a
# Python and a shell test that each run `go list -deps` with no offline rule are refused and named
# with a GitHub annotation, and the offline form and the reasoned form of each pass, over a count
# that shows the scan saw the command; a reason six lines up is too far; an argv array and a shell
# command whose go names a directory with -C before its subcommand count too; a go command in a
# comment, a quoted string or a heredoc's body starts nothing. Each form its header names is
# planted once and refused bare: an argv Biome writes one element a line (Go, TypeScript, Python),
# exec.CommandContext with a context made by a call, a command beside a neighbour whose Env holds
# GOPROXY=off, the go binary as filepath.Join(runtime.GOROOT(), "bin", "go"), a path ending /go,
# exec.LookPath, Bun.which, shutil.which or a variable, a shell's -c, eval, a heredoc fed to bash,
# os.system, subprocess's string, shell=True and an f-string, execSync and exec, Bun's $`...` with
# cd before go and with -C and a substitution, env SETTING go in an argv, a "GOPROXY=off" in a
# trailing comment, GOPROXY=off before the inherited environment or before a later GOPROXY, the
# later command of a pipeline, an export inside another function, env -u GOPROXY, and a .mts
# test; and the offline form of the environment each language binds to its own command passes.
# Its file reason: refused in a test file, and in a file ci.yaml or a workflow ci.yaml calls names,
# but not in one only a workflow outside that set or a comment names. Its second rule: a job of any
# workflow that runs internal/daemon's tests, which build envoy commands through
# testbuild.EnvoyBinary, is refused when no step before that one runs `.github/scripts/go-fetch.sh
# deps packages/envoy` of every such command - a step with none, one after, one that leaves a
# command out, one in another job - and passes when one does; the tests a step runs through a
# script and the library it sources, after a cd, with -C, or through bash -c count; a compile to
# /dev/null runs no test. Its other half: a step that reaches an offline go build - in a script it
# runs, in a function a sourced library defines and the script calls with its module and packages,
# or in a script the TypeScript of a Playwright run names - is refused with no fetch of those
# packages before it, or one that leaves a package out, and passes after one, or with a reason. Its
# third rule: a required job whose fetch step comes before a restore of ~/go/pkg/mod, or after a
# restore of another path, is refused, and one whose restore comes first passes and is listed. A
# tree with no file of a language, a tree with testbuild and no caller, and a tree with no ci.yaml
# each give UNKNOWN and exit 2.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root. No go command of a fixture runs: the script reads text.
# It needs a python3 with PyYAML, as the script does.
#
# Run from anywhere: .github/scripts/check-go-offline.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-go-offline.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# put <root> <path>: writes this harness's stdin to <path> under <root>, making its directory.
put() {
  mkdir -p "$(dirname "$1/$2")"
  cat >"$1/$2"
}

# fixture <name>: a tree the script can scan: the script linked in, a ci.yaml with one job, and one
# file of each of the four languages that starts no go command. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts"
  ln -s "$check_script" "$root/.github/scripts/check-go-offline.sh"
  put "$root" .github/workflows/ci.yaml <<'EOF'
name: CI
on: pull_request
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - run: echo lint
EOF
  printf 'package app\n\nfunc App() {}\n' | put "$root" packages/app/app.go
  printf 'export const app = 1;\n' | put "$root" packages/web/app.ts
  printf 'print("app")\n' | put "$root" tools/app.py
  printf '#!/usr/bin/env bash\necho app\n' | put "$root" tools/app.sh
  echo "$root"
}

# daemon_fixture <name>: a fixture with testbuild and a daemon test that builds two envoy commands
# through it. Echoes its root.
daemon_fixture() {
  local root
  root=$(fixture "$1")
  printf 'package testbuild\n\nfunc EnvoyBinary(t any, pkg string) string { return pkg }\n' |
    put "$root" packages/daemon/internal/testbuild/testbuild.go
  put "$root" packages/daemon/internal/daemon/daemon_test.go <<'EOF'
package daemon

import (
	"testing"

	"example.com/daemon/internal/testbuild"
)

func TestBuildsTheCommands(t *testing.T) {
	_ = testbuild.EnvoyBinary(t, "./cmd/dispatch")
	_ = testbuild.EnvoyBinary(t, "./cmd/listener")
}
EOF
  mkdir -p "$root/packages/envoy"
  echo "$root"
}

# nightly <root>: a workflow outside the required set holding one job, from this harness's stdin
# (the job's steps).
nightly() {
  {
    printf 'name: Nightly\non: schedule\njobs:\n  proof:\n    runs-on: ubuntu-24.04\n    steps:\n'
    cat
  } | put "$1" .github/workflows/nightly.yaml
}

run_check() { # run_check <root>
  set +e
  out=$("$1/.github/scripts/check-go-offline.sh" 2>&1)
  status=$?
  set -e
}

refused() { # refused <path> <line>: the checks of a site refused with its annotation
  check "fails" "$(is "$status" 1)"
  check "names $1:$2" "$(contains "$out" "$1:$2: a go command with no GOPROXY=off and no go-offline reason")"
  check "annotates the line" "$(contains "$out" "::error file=$1,line=$2::")"
}

# refuses <name> <path> <line> [<sites>]: a fixture holding this harness's stdin at <path> is
# refused at <line> and nowhere else, over <sites> go commands (1 when not given).
refuses() {
  local root
  root=$(fixture "$1")
  put "$root" "$2"
  run_check "$root"
  refused "$2" "$3"
  check "refuses it alone, over ${4:-1} go commands" "$(contains "$out" "^1 problems over the ${4:-1} go commands")"
}

# passes_offline <name> <path>: a fixture holding this harness's stdin at <path> passes, its one go
# command read as offline.
passes_offline() {
  local root
  root=$(fixture "$1")
  put "$root" "$2"
  run_check "$root"
  check "exits 0" "$(is "$status" 0)"
  check "reads its one command as offline" "$(contains "$out" 'every one of the 1 go commands .* runs with GOPROXY=off (1)')"
}

# --- The first rule, in each language ------------------------------------------------------------

echo "case: a Go test that runs go list -deps with no rule is refused, with an annotation"
root=$(fixture go-bare)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	if err := exec.Command("go", "list", "-deps", "./...").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
refused packages/app/deps_test.go 9
check "counts it against the total" "$(contains "$out" '1 problems over the 1 go commands')"

echo "case: the Go test with GOPROXY=off in the command's environment passes"
root=$(fixture go-offline)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os"
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as offline" "$(contains "$out" 'every one of the 1 go commands .* runs with GOPROXY=off (1), carries a reason (0)')"

echo "case: the Go test with a reason on the line above passes"
root=$(fixture go-reasoned)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

func TestDeps(t *testing.T) {
	// go-offline: it lists the standard library, which reads no module.
	if err := exec.Command("go", "list", "-deps", "std").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a reason six lines above the Go command is too far"
root=$(fixture go-distant)
put "$root" packages/app/deps_test.go <<'EOF'
package app

import (
	"os/exec"
	"testing"
)

// go-offline: it lists the standard library, which reads no module.
func TestDeps(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Log("listing")
	t.Log("the standard library")
	if err := exec.Command("go", "list", "-deps", "std").Run(); err != nil {
		t.Fatal(err)
	}
}
EOF
run_check "$root"
refused packages/app/deps_test.go 14

echo "case: a TypeScript test that runs go list -deps with no rule is refused"
root=$(fixture ts-bare)
put "$root" packages/web/deps.test.ts <<'EOF'
import { expect, test } from "bun:test";

test("deps", () => {
  const listed = Bun.spawnSync(["go", "list", "-deps", "./..."]);
  expect(listed.exitCode).toBe(0);
});
EOF
run_check "$root"
refused packages/web/deps.test.ts 4

echo "case: the TypeScript test with GOPROXY: \"off\" in the spawn's environment passes"
root=$(fixture ts-offline)
put "$root" packages/web/deps.test.ts <<'EOF'
import { expect, test } from "bun:test";

test("deps", () => {
  const listed = Bun.spawnSync(["go", "list", "-deps", "./..."], {
    env: { ...process.env, GOPROXY: "off" },
  });
  expect(listed.exitCode).toBe(0);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as offline" "$(contains "$out" 'runs with GOPROXY=off (1), carries a reason (0)')"

echo "case: the TypeScript test with a reason on the line above passes"
root=$(fixture ts-reasoned)
put "$root" packages/web/deps.test.ts <<'EOF'
import { expect, test } from "bun:test";

test("deps", () => {
  // go-offline: it lists the standard library, which reads no module.
  const listed = Bun.spawnSync(["go", "list", "-deps", "std"]);
  expect(listed.exitCode).toBe(0);
});
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a TypeScript argv array with -C and a directory before its subcommand is a site"
root=$(fixture ts-chdir)
put "$root" packages/web/deps.test.ts <<'EOF'
import { expect, test } from "bun:test";

test("deps", () => {
  const argv = ["go", "-C", moduleDir, "list", "-deps", "./..."];
  expect(Bun.spawnSync(argv).exitCode).toBe(0);
});
EOF
run_check "$root"
refused packages/web/deps.test.ts 4

echo "case: a Python test that runs go list -deps with no rule is refused"
root=$(fixture py-bare)
put "$root" tools/test_deps.py <<'EOF'
import subprocess


def test_deps():
    subprocess.run(["go", "list", "-deps", "./..."], check=True)
EOF
run_check "$root"
refused tools/test_deps.py 5

echo "case: the Python test with \"GOPROXY\": \"off\" in the run's environment passes"
root=$(fixture py-offline)
put "$root" tools/test_deps.py <<'EOF'
import os
import subprocess


def test_deps():
    subprocess.run(["go", "list", "-deps", "./..."], check=True,
                   env={**os.environ, "GOPROXY": "off"})
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as offline" "$(contains "$out" 'runs with GOPROXY=off (1), carries a reason (0)')"

echo "case: the Python test with a reason on the line above passes"
root=$(fixture py-reasoned)
put "$root" tools/test_deps.py <<'EOF'
import subprocess


def test_deps():
    # go-offline: it lists the standard library, which reads no module.
    subprocess.run(["go", "list", "-deps", "std"], check=True)
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a shell test that runs go list -deps with no rule is refused"
root=$(fixture sh-bare)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
go list -deps ./... >/dev/null
EOF
run_check "$root"
refused tools/deps.test.sh 3

echo "case: the shell test with GOPROXY=off on the command passes, and so does an export of it above"
root=$(fixture sh-offline)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
GOPROXY=off go list -deps ./... >/dev/null
EOF
put "$root" tools/exported.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
export GOPROXY=off
go list -deps ./... >/dev/null
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts both commands as offline" "$(contains "$out" 'runs with GOPROXY=off (2), carries a reason (0)')"

echo "case: the shell test with a reason on the line above passes"
root=$(fixture sh-reasoned)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# go-offline: it lists the standard library, which reads no module.
go list -deps std >/dev/null
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a shell go -C with a quoted directory before its subcommand is a site"
root=$(fixture sh-chdir)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
go -C "$root/packages/app" list -deps ./... >/dev/null
EOF
run_check "$root"
refused tools/deps.test.sh 3

echo "case: a go command in a comment, a quoted string or a heredoc's body starts nothing"
root=$(fixture sh-prose)
put "$root" tools/prose.test.sh <<'EOF'
#!/usr/bin/env bash
# go list -deps ./... in a comment starts nothing.
echo "go list -deps ./..."
cat <<'TEXT'
go list -deps ./...
TEXT
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no command at all" "$(contains "$out" 'every one of the 0 go commands')"

# --- Each form the header names, read a statement at a time -------------------------------------
# One plant a case, each bare (no GOPROXY=off, no reason) unless the case says otherwise. The Go
# plants need not compile: the script reads text.

echo "case: Go: an argv Biome writes one element a line is one site"
refuses go-biome packages/app/biome_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	list := exec.Command(
		"go",
		"list",
		"-deps",
		"./...",
	)
	_ = list.Run()
}
EOF

echo "case: Go: exec.CommandContext whose context is a call"
refuses go-context packages/app/context_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.CommandContext(ctxOf(t), "go", "build", "./...").Run()
}
EOF

echo "case: Go: a command does not borrow the GOPROXY=off of the next one's Env"
refuses go-neighbour packages/app/neighbour_test.go 4 2 <<'EOF'
package app

func TestDeps(t *testing.T) {
	list := exec.Command("go", "list", "-deps", "./...")
	_ = list.Run()
	build := exec.Command("go", "build", "./...")
	build.Env = append(os.Environ(), "GOPROXY=off")
	_ = build.Run()
}
EOF

echo "case: Go: the go binary as filepath.Join(runtime.GOROOT(), \"bin\", \"go\")"
refuses go-goroot packages/app/goroot_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "./...").Run()
}
EOF

echo "case: Go: the go binary as a literal path ending /go"
refuses go-path packages/app/path_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("/usr/local/go/bin/go", "build", "./...").Run()
}
EOF

echo "case: Go: the go binary exec.LookPath found, started in the same function"
refuses go-lookpath packages/app/lookpath_test.go 8 <<'EOF'
package app

func TestDeps(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	_ = exec.Command(goBin, "build", "./...").Run()
}
EOF

echo "case: Go: an exec.LookPath(\"go\") nothing here starts resolves the go a later line starts"
refuses go-lookpath-alone packages/app/resolve_test.go 4 <<'EOF'
package app

func goBinary(t *testing.T) string {
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return path
}
EOF

echo "case: Go: a go bound to a variable in the same function"
refuses go-variable packages/app/variable_test.go 5 <<'EOF'
package app

func TestDeps(t *testing.T) {
	goBin := "go"
	_ = exec.Command(goBin, "build", "./...").Run()
}
EOF

echo "case: Go: a shell's -c whose script runs go"
refuses go-bash-c packages/app/bash_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("bash", "-c", "cd sub && go build ./...").Run()
}
EOF

echo "case: Go: go after env in an argv, with its own settings"
refuses go-env-argv packages/app/env_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("env", "GOFLAGS=-mod=mod", "go", "build", "./...").Run()
}
EOF
passes_offline go-env-argv-offline packages/app/env_test.go <<'EOF'
package app

func TestDeps(t *testing.T) {
	_ = exec.Command("env", "GOPROXY=off", "go", "build", "./...").Run()
}
EOF

echo "case: Go: a \"GOPROXY=off\" in a trailing comment sets nothing"
refuses go-comment packages/app/comment_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...") // "GOPROXY=off"
	_ = build.Run()
}
EOF

echo "case: Go: GOPROXY=off before the inherited environment, or before a later GOPROXY, is overridden"
refuses go-before-environ packages/app/before_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...")
	build.Env = append([]string{"GOPROXY=off"}, os.Environ()...)
	_ = build.Run()
}
EOF
refuses go-later-goproxy packages/app/later_test.go 4 <<'EOF'
package app

func TestDeps(t *testing.T) {
	build := exec.Command("go", "build", "./...")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOPROXY=https://proxy.golang.org")
	_ = build.Run()
}
EOF

echo "case: Go: an Env bound to a variable that ends in GOPROXY=off is offline"
passes_offline go-bound-env packages/app/bound_test.go <<'EOF'
package app

func TestDeps(t *testing.T) {
	env := append(os.Environ(), "GOPROXY=off")
	build := exec.Command("go", "build", "./...")
	build.Env = env
	_ = build.Run()
}
EOF

echo "case: TypeScript: the cmd of a Bun.spawnSync Biome writes one element a line"
refuses ts-biome-cmd packages/web/biome.test.ts 2 <<'EOF'
test("build", () => {
  Bun.spawnSync({
    cmd: [
      "go",
      "build",
      "./cmd/listener",
    ],
  });
});
EOF

echo "case: TypeScript: an argv Biome writes one element a line"
refuses ts-biome-argv packages/web/argv.test.ts 2 <<'EOF'
test("deps", () => {
  Bun.spawnSync(
    [
      "go",
      "list",
      "-deps",
      "./...",
    ],
    { cwd: dir }
  );
});
EOF

echo "case: TypeScript: a spawn does not borrow the next one's GOPROXY: \"off\""
refuses ts-neighbour packages/web/neighbour.test.ts 2 2 <<'EOF'
test("deps", () => {
  Bun.spawnSync(["go", "list", "-deps", "./..."]);
  Bun.spawnSync(["go", "build", "./..."], {
    env: { ...process.env, GOPROXY: "off" },
  });
});
EOF

echo "case: TypeScript: GOPROXY: \"off\" before a spread of the environment is overridden"
refuses ts-before-spread packages/web/spread.test.ts 2 <<'EOF'
test("build", () => {
  Bun.spawnSync(["go", "build", "./..."], {
    env: { GOPROXY: "off", ...process.env },
  });
});
EOF

echo "case: TypeScript: execSync of a shell string that runs go after cd"
refuses ts-execsync packages/web/execsync.test.ts 2 <<'EOF'
test("build", () => {
  execSync("cd sub && go build ./...");
});
EOF

echo "case: TypeScript: exec of a go command (Node's child_process)"
refuses ts-exec packages/web/exec.test.ts 2 <<'EOF'
test("deps", () => {
  exec("go list -deps ./...", () => {});
});
EOF

echo "case: TypeScript: Bun's \$\`go ...\`"
refuses ts-bun-shell packages/web/shell.test.ts 2 <<'EOF'
test("deps", async () => {
  await $`go list -deps ./...`;
});
EOF

echo "case: TypeScript: Bun's \$\` with cd before go, and with -C and a substitution"
refuses ts-bun-shell-cd packages/web/shellcd.test.ts 2 <<'EOF'
test("build", async () => {
  await $`cd ${dir} && go build ./...`;
});
EOF
refuses ts-bun-shell-chdir packages/web/shellchdir.test.ts 2 <<'EOF'
test("build", async () => {
  await $`go -C ${dir} build ./cmd/listener`;
});
EOF

echo "case: TypeScript: Bun's \$\` with a .env({...}) that ends in GOPROXY: \"off\" is offline"
passes_offline ts-bun-shell-env packages/web/shellenv.test.ts <<'EOF'
test("deps", async () => {
  await $`go list -deps ./...`.env({ ...process.env, GOPROXY: "off" });
});
EOF

echo "case: TypeScript: a go Bun.which found, bound to a name"
refuses ts-which packages/web/which.test.ts 3 <<'EOF'
test("build", () => {
  const go = Bun.which("go");
  Bun.spawn([go!, "build", "./..."]);
});
EOF

echo "case: TypeScript: go after env in an argv, and a shell's -c"
refuses ts-env-argv packages/web/env.test.ts 2 <<'EOF'
test("build", () => {
  Bun.spawnSync(["env", "GOFLAGS=-mod=mod", "go", "build", "./..."]);
});
EOF
refuses ts-bash-c packages/web/bash.test.ts 2 <<'EOF'
test("build", () => {
  Bun.spawnSync(["bash", "-c", "go build ./..."]);
});
EOF

echo "case: TypeScript: Node's spawnSync(\"go\", [...]) with GOPROXY: \"off\" in its own env is offline"
passes_offline ts-node-spawn packages/web/node.test.ts <<'EOF'
test("deps", () => {
  spawnSync("go", ["list", "./..."], { env: { ...process.env, GOPROXY: "off" } });
});
EOF

echo "case: TypeScript: a .mts test is read"
refuses ts-mts packages/web/deps.test.mts 2 <<'EOF'
test("deps", () => {
  Bun.spawnSync(["go", "list", "-deps", "./..."]);
});
EOF

echo "case: Python: an argv Biome-style, one element a line"
refuses py-multiline tools/test_multiline.py 6 <<'EOF'
import subprocess


def test_deps():
    subprocess.run(
        [
            "go",
            "list",
            "-deps",
        ]
    )
EOF

echo "case: Python: os.system, subprocess's string, shell=True and an f-string"
refuses py-system tools/test_system.py 5 <<'EOF'
import os


def test_deps():
    os.system("go list -deps ./...")
EOF
refuses py-string tools/test_string.py 5 <<'EOF'
import subprocess


def test_deps():
    subprocess.run("go list -deps ./...")
EOF
refuses py-shell tools/test_shell.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run("cd sub && go build ./...", shell=True)
EOF
refuses py-fstring tools/test_fstring.py 5 <<'EOF'
import subprocess


def test_deps(pkg):
    subprocess.run(f"go list -deps {pkg}", shell=True)
EOF

echo "case: Python: GOPROXY off before a ** spread is overridden; dict(os.environ, GOPROXY=\"off\") is offline"
refuses py-before-spread tools/test_spread.py 5 <<'EOF'
import os, subprocess


def test_deps():
    subprocess.run(["go", "list"], env={"GOPROXY": "off", **os.environ})
EOF
passes_offline py-dict tools/test_dict.py <<'EOF'
import os, subprocess


def test_deps():
    subprocess.run(["go", "list"], env=dict(os.environ, GOPROXY="off"))
EOF

echo "case: Python: a shell's -c in an argv, go after env, and a go shutil.which found"
refuses py-bash-c tools/test_bash.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(["bash", "-c", "go build ./..."])
EOF
refuses py-env-argv tools/test_env.py 5 <<'EOF'
import subprocess


def test_build():
    subprocess.run(["env", "GOFLAGS=-mod=mod", "go", "build"])
EOF
refuses py-which tools/test_which.py 6 <<'EOF'
import shutil, subprocess


def test_build():
    go = shutil.which("go")
    subprocess.run([go, "build", "./..."])
EOF

echo "case: shell: bash -c, eval, and a heredoc a shell reads"
refuses sh-bash-c tools/bash.test.sh 2 <<'EOF'
#!/usr/bin/env bash
bash -c 'go list -deps ./...'
EOF
refuses sh-eval tools/eval.test.sh 2 <<'EOF'
#!/usr/bin/env bash
eval "go list -deps ./..."
EOF
refuses sh-heredoc tools/heredoc.test.sh 3 <<'EOF'
#!/usr/bin/env bash
bash <<'SCRIPT'
go list -deps ./...
SCRIPT
EOF

echo "case: shell: the later command of a pipeline needs its own GOPROXY=off"
refuses sh-pipeline tools/pipeline.test.sh 3 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off go list -deps ./... |
  xargs go build
EOF
refuses sh-pipeline-line tools/pipeline-line.test.sh 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off go list -deps ./... | xargs go build
EOF

echo "case: shell: an export GOPROXY=off inside a function covers that function alone"
refuses sh-function-export tools/function.test.sh 6 <<'EOF'
#!/usr/bin/env bash
setup() {
  export GOPROXY=off
}
setup
go list -deps ./...
EOF
passes_offline sh-same-function tools/function.test.sh <<'EOF'
#!/usr/bin/env bash
build() {
  export GOPROXY=off
  go build ./...
}
EOF

echo "case: shell: the go binary as a path ending /go, and as a variable set to it"
refuses sh-goroot tools/goroot.test.sh 2 <<'EOF'
#!/usr/bin/env bash
"$GOROOT/bin/go" list -deps ./...
EOF
refuses sh-variable tools/variable.test.sh 3 <<'EOF'
#!/usr/bin/env bash
GO=$(command -v go)
"$GO" build ./...
EOF

echo "case: shell: env -u GOPROXY and a later GOPROXY undo an earlier GOPROXY=off"
refuses sh-env-unset tools/unset.test.sh 3 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off
env -u GOPROXY go list -deps ./...
EOF
refuses sh-later tools/later.test.sh 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off GOPROXY=direct go list -deps ./...
EOF

# --- The file reason ------------------------------------------------------------------------------

echo "case: a go-offline-file reason in a test file is refused, and its command with it"
root=$(fixture file-reason-test)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, saying why" \
  "$(contains "$out" 'tools/deps.test.sh:1: a go-offline-file reason, which this file may not carry: it is a test file')"
check "and refuses the command it would cover" "$(contains "$out" 'tools/deps.test.sh:3: a go command with no GOPROXY=off')"

echo "case: a go-offline-file reason in a file ci.yaml names is refused"
root=$(fixture file-reason-ci)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '      - run: bash tools/list-deps.sh\n' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, naming ci.yaml" \
  "$(contains "$out" 'tools/list-deps.sh:1: a go-offline-file reason, which this file may not carry: a required workflow names it (.github/workflows/ci.yaml)')"
check "and refuses the command" "$(contains "$out" 'tools/list-deps.sh:3: a go command with no GOPROXY=off')"

echo "case: a go-offline-file reason in a file a workflow ci.yaml calls names is refused"
root=$(fixture file-reason-called)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '  checks:\n    uses: ./.github/workflows/ci-job-checks.yaml\n' >>"$root/.github/workflows/ci.yaml"
put "$root" .github/workflows/ci-job-checks.yaml <<'EOF'
name: CI job checks
on: workflow_call
jobs:
  checks:
    runs-on: ubuntu-24.04
    steps:
      - run: tools/list-deps.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses the reason, naming the called workflow" \
  "$(contains "$out" 'a required workflow names it (.github/workflows/ci-job-checks.yaml)')"

echo "case: a go-offline-file reason in a file only a workflow outside the required set, and a comment of ci.yaml, name passes"
root=$(fixture file-reason-allowed)
put "$root" tools/list-deps.sh <<'EOF'
#!/usr/bin/env bash
# go-offline-file: a person runs this by hand.
go list -deps ./... >/dev/null
EOF
printf '      # tools/list-deps.sh is a person'"'"'s.\n' >>"$root/.github/workflows/ci.yaml"
nightly "$root" <<'EOF'
      - run: bash tools/list-deps.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as covered by its file's reason" "$(contains "$out" 'is in a file whose reason covers it (1)')"

# --- The second rule: a job that runs testbuild's tests fetches their commands first ---------------

echo "case: a job that runs internal/daemon's tests with no fetch step is refused"
root=$(daemon_fixture job-bare)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the workflow, the job and the step" \
  "$(contains "$out" '.github/workflows/nightly.yaml:1: job proof, step "Test the daemon", runs the tests of packages/daemon/internal/daemon')"
check "names the fetch to add, with both commands" \
  "$(contains "$out" 'add `.github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener` before it')"

echo "case: the same job with the fetch step before the tests passes"
root=$(daemon_fixture job-fetched)
nightly "$root" <<'EOF'
      - name: Fetch the modules of the commands the tests build
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" "each of the 1 steps that run testbuild's tests comes after its fetch")"

echo "case: a fetch step after the tests is refused"
root=$(daemon_fixture job-late)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
      - name: Fetch the modules of the commands the tests build
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" "$(contains "$out" 'job proof, step "Test the daemon"')"

echo "case: a fetch step that leaves a command out is refused"
root=$(daemon_fixture job-partial)
nightly "$root" <<'EOF'
      - name: Fetch the modules of the Dispatch server
        run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step" "$(contains "$out" 'job proof, step "Test the daemon"')"

echo "case: a fetch step in another job does not count"
root=$(daemon_fixture job-other)
nightly "$root" <<'EOF'
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
  fetch:
    runs-on: ubuntu-24.04
    steps:
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job without the fetch" "$(contains "$out" 'job proof, step "Test the daemon"')"

# As e2e.yaml's proof job runs them: the step runs an entry script, which sources the library that
# compiles the daemon's tests into a driver.
echo "case: a step that runs the tests through a script and the library it sources is refused, and passes after the fetch"
root=$(daemon_fixture job-script)
put "$root" scripts/e2e/proof.sh <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/../.." && pwd)
. "$root/scripts/e2e/lib/proof.sh"
EOF
put "$root" scripts/e2e/lib/proof.sh <<'EOF'
# go-offline-file: a person runs the proof that sources this, and the nightly job.
go -C "$root/packages/daemon" test -c -o "$work/driver" ./internal/daemon
EOF
nightly "$root" <<'EOF'
      - name: Run the proof
        run: bash scripts/e2e/proof.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the step that runs the script" "$(contains "$out" 'job proof, step "Run the proof", runs the tests of packages/daemon/internal/daemon')"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Run the proof
        run: bash scripts/e2e/proof.sh
EOF
run_check "$root"
check "passes once the job fetches first" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"

# needs_fetch <name> <step name> <steps> [<script>]: a job of those steps alone is refused, naming
# the step, and the same job with the fetch step first passes, judging that one step. <script>,
# when given, is written to scripts/daemon-tests.sh in both trees.
needs_fetch() {
  local root fetch='      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener'
  root=$(daemon_fixture "$1")
  printf '%s\n' "$3" | nightly "$root"
  [ -z "${4:-}" ] || printf '%s\n' "$4" | put "$root" scripts/daemon-tests.sh
  run_check "$root"
  check "fails without the fetch" "$(is "$status" 1)"
  check "names the step" "$(contains "$out" "job proof, step \"$2\", runs the tests of packages/daemon/internal/daemon")"
  root=$(daemon_fixture "$1-fetched")
  printf '%s\n%s\n' "$fetch" "$3" | nightly "$root"
  [ -z "${4:-}" ] || printf '%s\n' "$4" | put "$root" scripts/daemon-tests.sh
  run_check "$root"
  check "passes with the fetch before it" "$(is "$status" 0)"
  check "judges the step" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"
}

echo "case: a step that runs the tests after a cd on its own line is judged"
needs_fetch job-cd "Test the daemon after a cd" "$(cat <<'EOF'
      - name: Test the daemon after a cd
        run: cd packages/daemon && go test ./internal/daemon/
EOF
)"

echo "case: a step that runs the tests with go test -C is judged"
needs_fetch job-test-chdir "Test the daemon with go test -C" "$(cat <<'EOF'
      - name: Test the daemon with go test -C
        run: go test -C packages/daemon ./internal/daemon/
EOF
)"

echo "case: a step whose script runs the tests in a ( cd ... && go test ) group is judged"
needs_fetch job-script-cd "Run the tests of the daemon" "$(cat <<'EOF'
      - name: Run the tests of the daemon
        run: bash scripts/daemon-tests.sh
EOF
)" "$(cat <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/.." && pwd)
(cd "$root/packages/daemon" && GOPROXY=off go test ./internal/daemon/)
EOF
)"

echo "case: a step that compiles the tests to /dev/null runs none and needs no fetch"
root=$(daemon_fixture job-compile)
nightly "$root" <<'EOF'
      - name: Compile the daemon's tests
        run: go test -c -o /dev/null ./internal/daemon/
        working-directory: packages/daemon
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/dispatch ./cmd/listener
      - name: Test the daemon
        run: go test ./internal/daemon/
        working-directory: packages/daemon
EOF
run_check "$root"
check "exits 0, the compile before the fetch refused nothing" "$(is "$status" 0)"
check "judges the test step alone" "$(contains "$out" "each of the 1 steps that run testbuild's tests")"

echo "case: a step that runs the tests through bash -c is judged"
needs_fetch job-bash-c "Test the daemon through bash -c" "$(cat <<'EOF'
      - name: Test the daemon through bash -c
        run: bash -c 'cd packages/daemon && go test ./internal/daemon/'
EOF
)"

# --- The second rule's other half: every offline build a step reaches ---------------------------
# A job of any workflow whose step reaches a go build made with GOPROXY=off - in its run text, in
# a script it runs or sources, in a function such a script calls, or in a script the TypeScript of
# a bun or Playwright test run names - fetches what that build builds in a step before.

# broker_script <root>: scripts/run-broker.sh, which builds two commands of packages/envoy offline
# from its own directory, as the Dispatch launchers do.
broker_script() {
  mkdir -p "$1/packages/envoy"
  put "$1" scripts/run-broker.sh <<'EOF'
#!/usr/bin/env bash
cd "$(dirname "$0")/../packages/envoy"
env GOPROXY=off go build -o ./out/ ./cmd/broker ./cmd/dev-oidc-issuer
EOF
}

echo "case: a job whose step runs a script that builds offline, with no fetch before it, is refused"
root=$(fixture offline-bare)
broker_script "$root"
nightly "$root" <<'EOF'
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job, the step, the build and its packages" \
  "$(contains "$out" 'job proof, step "Start the broker", reaches scripts/run-broker.sh:3, an offline go command of packages/envoy: ./cmd/broker ./cmd/dev-oidc-issuer')"
check "names the fetch to add" "$(contains "$out" 'add `.github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker ./cmd/dev-oidc-issuer`')"

echo "case: the same job passes after a fetch of both commands, and not after a fetch of one"
root=$(fixture offline-fetched)
broker_script "$root"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker ./cmd/dev-oidc-issuer
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the step it judged" "$(contains "$out" 'each of the 1 steps that reach an offline build comes after a fetch')"
root=$(fixture offline-partial)
broker_script "$root"
nightly "$root" <<'EOF'
      - run: .github/scripts/go-fetch.sh deps packages/envoy ./cmd/broker
      - name: Start the broker
        run: bash scripts/run-broker.sh
EOF
run_check "$root"
check "fails when the fetch leaves a command out" "$(is "$status" 1)"
check "naming the one it leaves out" "$(contains "$out" 'reaches scripts/run-broker.sh:3')"

echo "case: a build in a sourced function, called with its module and packages, is judged"
root=$(fixture offline-function)
mkdir -p "$root/packages/envoy"
put "$root" lib/go-build-offline.sh <<'EOF'
go_build_offline() {
  local dir=$1 out=$2 root
  shift 2
  root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
  [[ "$dir" == /* ]] || dir="$root/$dir"
  (cd -- "$dir" && GOPROXY=off go build -o "$out" "$@")
}
EOF
put "$root" scripts/start.sh <<'EOF'
#!/usr/bin/env bash
. "$(dirname "$0")/../lib/go-build-offline.sh"
go_build_offline packages/envoy ./bin/ ./cmd/listener
EOF
nightly "$root" <<'EOF'
      - name: Start the listener
        run: scripts/start.sh
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the build in the function, with the caller's module and package" \
  "$(contains "$out" 'reaches lib/go-build-offline.sh:6, an offline go command of packages/envoy: ./cmd/listener')"

echo "case: a Playwright run whose TypeScript names a script that builds offline is judged"
root=$(fixture offline-harness)
mkdir -p "$root/packages/envoy"
put "$root" packages/dispatch/e2e/harness.ts <<'EOF'
export const serverScript = join(import.meta.dir, "run-server.sh");
EOF
put "$root" packages/dispatch/e2e/run-server.sh <<'EOF'
#!/usr/bin/env bash
cd "$(dirname "$0")/../../envoy"
env GOPROXY=off go build -o ./dispatch-e2e ./cmd/dispatch
EOF
nightly "$root" <<'EOF'
      - name: Run the browser suite
        run: bunx playwright test --config e2e/playwright.config.ts
        working-directory: packages/dispatch
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the script the harness starts" \
  "$(contains "$out" 'step "Run the browser suite", reaches packages/dispatch/e2e/run-server.sh:3, an offline go command of packages/envoy: ./cmd/dispatch')"

echo "case: a step that says why it needs no fetch passes, counted as such"
root=$(fixture offline-reasoned)
broker_script "$root"
nightly "$root" <<'EOF'
      - name: Start the broker
        run: |
          # go-offline: the binaries job built the broker; DISPATCH_E2E_BROKER_BIN_DIR names it.
          bash scripts/run-broker.sh
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts its reason" "$(contains "$out" '(1 say why they need none)')"

# --- Item 1's rule: a fetch step of a required job comes after the job's module cache ------------

echo "case: a required job whose fetch step comes before its module cache restore is refused"
root=$(fixture fetch-first)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
      - uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the job and the fetch step" \
  "$(contains "$out" 'job lint, step "Fetch the modules the tests build", runs go-fetch.sh with no step before it that restores ~/go/pkg/mod')"

echo "case: the same job with the restore first passes, and lists the fetch step with that restore"
root=$(fixture fetch-after)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - name: Restore the module cache
        uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/pkg/mod
            ~/.cache/go-build
          key: go-app
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "lists the pair" \
  "$(contains "$out" '^  .github/workflows/ci.yaml job lint: "Fetch the modules the tests build" after "Restore the module cache"')"
check "counts it" "$(contains "$out" 'each of the 1 fetch steps of a required job comes after a restore of ~/go/pkg/mod')"

echo "case: a restore of another path does not count"
root=$(fixture fetch-other-cache)
cat >>"$root/.github/workflows/ci.yaml" <<'EOF'
      - uses: actions/cache@v4
        with:
          path: ~/.cache/go-build
          key: go-build
      - name: Fetch the modules the tests build
        run: .github/scripts/go-fetch.sh deps packages/app ./cmd/app
EOF
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the fetch step" "$(contains "$out" 'step "Fetch the modules the tests build", runs go-fetch.sh with no step before it')"

# --- A scan that cannot see -----------------------------------------------------------------------

echo "case: a tree with no Python file gives UNKNOWN"
root=$(fixture blind-python)
rm "$root/tools/app.py"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the language it did not read" "$(contains "$out" '^UNKNOWN: read no py file under ')"

echo "case: a tree with testbuild and no test that calls it gives UNKNOWN"
root=$(daemon_fixture blind-testbuild)
rm "$root/packages/daemon/internal/daemon/daemon_test.go"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names testbuild" "$(contains "$out" '^UNKNOWN: packages/daemon/internal/testbuild/testbuild.go exists and no daemon test calls testbuild.EnvoyBinary')"

echo "case: a tree whose tests call testbuild and no step runs them gives UNKNOWN"
root=$(daemon_fixture blind-jobs)
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the callers no step was seen to run" \
  "$(contains "$out" '^UNKNOWN: packages/daemon/internal/daemon call testbuild.EnvoyBinary and no step of any workflow was seen to run their tests')"

echo "case: a tree with no ci.yaml gives UNKNOWN"
root=$(fixture blind-ci)
rm "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says no workflow is known to be required" "$(contains "$out" '^UNKNOWN: no .github/workflows/ci.yaml here')"

summary "check-go-offline.sh"
