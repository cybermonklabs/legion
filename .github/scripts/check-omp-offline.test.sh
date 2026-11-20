#!/usr/bin/env bash
# Tests for `check-omp-offline.sh`: a Go test that starts Oh My Pi in a function that never calls
# testbin's offline helper is refused, by its door (testbin.OMP) and by its exec.Command alike, and
# so is a TypeScript start whose environment calls neither ompOffline nor ompEnv; the helper form
# and the reasoned form of each pass; a reason on the start's own line or five lines up passes, and
# one six lines up is too far; a start spelled in a comment or a string starts nothing; a regular
# expression holding a backtick hides no start after it; a Sync start under the helper is refused,
# since the helper's listener cannot answer while the call holds the event loop; a helper that
# stopped naming HTTPS_PROXY, or an environment helper that stopped calling its own, fails; and a
# scan that read nothing, read no file of one language, or found no start where testbin or the
# harness exists, reports UNKNOWN and exits 2.
#
# Each form the check's header names has a row, which fails when the check stops seeing that form.
# The binary is followed by its value, whatever its name: a spawn of process.env.LEGION_TEST_OMP
# itself, a name bound to it, Bun.env, an env's [KEY] by the name or by a constant naming it,
# OMP_BIN, a function of the file or of the module it is imported from that returns it (a + or a
# template ending /omp included), a parameter its function's call is given, either part of ?? or
# ||, Bun.which, which, path.join, path.resolve, "omp", a literal path, and the names ompPath and
# binary; in Go exec.LookPath("omp"), filepath.Join(..., "omp"), a literal path, a function or
# method of the package or of the package that qualifies the call, LEGION_OMP_PATH, a parameter,
# and a binary from a helper that calls OMPOffline itself. The starts: Bun.spawn, Bun.spawnSync,
# spawn, execFile, execFileSync, execFileAsync, execa and execaSync whose program is the binary
# (after env, or a shell's -c), and exec, execSync, execAsync and Bun's $`...` of a shell string,
# whose omp is a start after nice, timeout, command, exec or env, in a $(...), after && or ;, or in
# a shell's -c, and no start as another program's argument; in Go exec.Command and
# exec.CommandContext, and inside testbin a start of its own OMP under its own OMPOffline. The
# helper counts only in the start's own call or environment: one in the next statement holds
# nothing, nor does one in the function that returns the program, and an environment the call
# names, or a closure is given at its every call, is followed to it; a Go door passes by a helper
# anywhere in its top-level function, its closures included. Test code is a Go _test.go file or a
# file of testbin, and a TypeScript or JavaScript .test. or .spec. file or a file under test/,
# tests/ or __tests__/; a start in a file that is no test code is not read, nor is one under
# testdata, node_modules, dist, .git or .jj. Last, the real repository passes over a count of
# starts it actually found.
#
# Each case builds a miniature tree under a temporary directory and symlinks the real script into
# it, so the script under test is the file CI runs: it resolves its root from its own path, which
# the symlink puts at the fixture root. The cases sit in check-omp-offline-cases-<part>.sh, one
# file per part of the check, which this file sources after its fixtures; the scan controls that
# must report UNKNOWN, and the real repository's run, are here.
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

# named <file>:<line>: whether the last run annotated that line.
named() { contains "$out" "^::error file=[^,]*$1,line=$2::"; }

echo "case: the helper form of each language passes, every start counted"
root=$(fixture helped)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the two Go starts and the TypeScript one" "$(contains "$out" '3 starts of Oh My Pi in 4 test files read (Go 2, TypeScript 1)')"
check "all three under the helper" "$(contains "$out" 'offline helper (3) or carries a reason (0)')"

# --- The cases, each file one part of the check ------------------------------------------------
# Each file this sources holds the cases of one part: Go's reading, TypeScript's reading of a start
# and its environment, how TypeScript follows the binary, the TypeScript command forms, which
# files are read, and the helpers themselves. They share this file's fixtures and counters.
for part in go typescript binary forms files helpers; do
  # shellcheck source=/dev/null
  source "$script_dir/check-omp-offline-cases-$part.sh"
done

# --- A scan that cannot see -----------------------------------------------------------------------

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

echo "case: Go tests with no TypeScript test file are UNKNOWN"
root=$(fixture go_only)
rm -r "$root/packages/pi-shared"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "names the language it could not read" "$(contains "$out" 'UNKNOWN: read no TypeScript test file')"

echo "case: the harness with no TypeScript start anywhere is UNKNOWN"
root=$(fixture no_ts_start)
put "$root" packages/pi-shared/test/omp-harness.ts <<'EOF'
import { ompOffline } from "./omp-offline";

function ompEnv(home: string, cleanup: (() => Promise<void>)[]): Record<string, string> {
  return { HOME: home, ...ompOffline(cleanup) };
}
EOF
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says the reading sees none" "$(contains "$out" 'UNKNOWN: packages/pi-shared/test/omp-harness.ts exists and no TypeScript test starts Oh My Pi')"

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
