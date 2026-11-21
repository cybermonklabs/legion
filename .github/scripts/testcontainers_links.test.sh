#!/usr/bin/env bash
# Tests for `testcontainers_links.py`: a command, or a test binary that starts itself, that links
# testcontainers-go fails the check, named with its import chain; every other binary passes it.
#
# Each case builds Go modules under a temporary directory and runs the real script over them, so
# the script under test is the file CI runs. A module there stands for testcontainers-go, under
# its own module path, and the fixture module reaches it through a `replace` of a directory, so no
# case downloads anything: every go command runs with GOPROXY=off, GOWORK=off and
# GOTOOLCHAIN=local.
#
# Run from anywhere: .github/scripts/testcontainers_links.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/testcontainers_links.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# gofile <path> <line>...: a file of those lines, its directory made first.
gofile() {
  local path=$1
  shift
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$@" >"$path"
}

# testcontainers <root>: the stand-in for testcontainers-go under <root>/tc, a module of that
# path whose package has an init, as the real one's bootstrap does.
testcontainers() {
  local root=$1
  gofile "$root/tc/go.mod" 'module github.com/testcontainers/testcontainers-go' '' 'go 1.22'
  gofile "$root/tc/tc.go" 'package testcontainers' '' 'var Processes int' '' 'func init() { Processes = 1 }'
}

# module <root> <name>: an empty module example.com/<name> at <root>/<name> that requires the
# stand-in through a replace of its directory.
module() {
  local root=$1 name=$2
  gofile "$root/$name/go.mod" "module example.com/$name" '' 'go 1.22' '' \
    'require github.com/testcontainers/testcontainers-go v0.0.0' '' \
    'replace github.com/testcontainers/testcontainers-go => ../tc'
}

# shapes <root> <name>: the module example.com/<name> holding one package of each shape the
# check reads. Nothing in it links the stand-in yet; each case adds what it tests.
#   internal/testpg   imports the stand-in, as internal/testpg does
#   internal/rig      imports testpg, as internal/cipher/ciphertest does
#   internal/client   imports nothing, as internal/cipher/ciphertest/podclient
#   cmd/seed          a command on client
#   internal/helper   a test helper that starts the test binary, as internal/stacktest does
shapes() {
  local root=$1 name=$2 m="$1/$2"
  module "$root" "$name"
  gofile "$m/internal/testpg/testpg.go" 'package testpg' '' \
    'import testcontainers "github.com/testcontainers/testcontainers-go"' '' \
    'func Count() int { return testcontainers.Processes }'
  gofile "$m/internal/rig/rig.go" 'package rig' '' "import \"example.com/$name/internal/testpg\"" '' \
    'func Start() int { return testpg.Count() }'
  gofile "$m/internal/client/client.go" 'package client' '' 'func Ask() string { return "pending" }'
  gofile "$m/cmd/seed/main.go" 'package main' '' "import \"example.com/$name/internal/client\"" '' \
    'func main() { println(client.Ask()) }'
  gofile "$m/internal/helper/helper.go" 'package helper' '' \
    'import (' '	"os"' '	"os/exec"' '	"testing"' ')' '' \
    'func Under(t *testing.T) { _ = exec.Command(os.Args[0], "-test.run", t.Name()) }'
}

# run_check <directory>...: runs the script under test from $work over the given module
# directories, and sets out (both streams) and status.
run_check() {
  set +e
  out=$(cd "$work" && env GOFLAGS= GOPROXY=off GOWORK=off GOTOOLCHAIN=local "$checker" "$@" 2>&1)
  status=$?
  set -e
}

testcontainers "$work"

echo "case: a module whose binaries link nothing passes, and says what it checked"
shapes "$work" clean
gofile "$work/clean/internal/client/client_test.go" 'package client' '' \
  'import (' '	"os"' '	"os/exec"' '	"testing"' ')' '' \
  'func TestStartsItself(t *testing.T) { _ = exec.Command(os.Args[0], "-test.run=^$").Run() }'
run_check clean
check "exits 0" "$(is "$status" 0)"
check "lists the test binary that starts itself, with the line that starts it" \
  "$(contains "$out" '^  example.com/clean/internal/client: clean/internal/client/client_test.go:9$')"
check "counts one command and one self-starting test binary" \
  "$(contains "$out" '^clean: 2 of 2 binaries link no testcontainers-go (1 commands, 1 self-starting test binaries)$')"
check "prints no error" "$(is "$(contains "$out" '::error::')" false)"

echo "case: a command that reaches the stand-in through the rig fails, named with its chain"
# Before the change of this check's first commit: requestseed imported the broker rig for its pod
# client, and the rig imports storetest, which imports testpg.
shapes "$work" command
gofile "$work/command/cmd/seed/main.go" 'package main' '' \
  'import (' '	"example.com/command/internal/client"' '	"example.com/command/internal/rig"' ')' '' \
  'func main() { println(client.Ask(), rig.Start()) }'
run_check command
check "exits 1" "$(is "$status" 1)"
check "names the command and the chain to the stand-in" \
  "$(contains "$out" '^::error::command: the command example.com/command/cmd/seed links testcontainers-go, and a command pays the init at every start. The import chain: example.com/command/cmd/seed -> example.com/command/internal/rig -> example.com/command/internal/testpg -> github.com/testcontainers/testcontainers-go$')"
check "says why, and what to move" "$(contains "$out" 'Move what the binary uses out of the package the chain goes through')"
check "counts it" "$(contains "$out" '^command: 0 of 1 binaries link no testcontainers-go')"

echo "case: a test binary that starts itself and links the stand-in fails; one that does not start itself passes"
shapes "$work" tests
# internal/rig's tests start their own binary and import testpg: refused.
gofile "$work/tests/internal/rig/rig_test.go" 'package rig' '' \
  'import (' '	"os"' '	"os/exec"' '	"testing"' ')' '' \
  'func TestChild(t *testing.T) {' '	self, _ := os.Executable()' '	_ = exec.Command(self).Run()' '}'
# internal/testpg's tests link the stand-in too, but name os.Args[0] only in a comment: they do
# not start themselves, so a test pays the init once and the binary is not held to the rule.
gofile "$work/tests/internal/testpg/testpg_test.go" 'package testpg' '' 'import "testing"' '' \
  '// os.Args[0] is the test binary; nothing here starts it.' \
  'func TestCount(t *testing.T) { _ = Count() }'
run_check tests
check "exits 1" "$(is "$status" 1)"
check "names the test binary, the line that starts it and the chain, its test files marked" \
  "$(contains "$out" '^::error::tests: the test binary of example.com/tests/internal/rig links testcontainers-go, and it starts itself (tests/internal/rig/rig_test.go:10). The import chain: example.com/tests/internal/rig.test -> example.com/tests/internal/rig (with its test files) -> example.com/tests/internal/testpg -> github.com/testcontainers/testcontainers-go$')"
check "does not count a name in a comment as a start" \
  "$(is "$(contains "$out" 'example.com/tests/internal/testpg:')" false)"
check "does not hold the testpg test binary to the rule" \
  "$(is "$(contains "$out" 'test binary of example.com/tests/internal/testpg')" false)"

echo "case: a test binary started by a test helper it imports is held to the rule"
shapes "$work" viahelper
gofile "$work/viahelper/internal/rig/rig_test.go" 'package rig_test' '' \
  'import (' '	"testing"' '' '	"example.com/viahelper/internal/helper"' '	"example.com/viahelper/internal/rig"' ')' '' \
  'func TestUnder(t *testing.T) { helper.Under(t); _ = rig.Start() }'
run_check viahelper
check "exits 1" "$(is "$status" 1)"
check "names the helper and the line in it that starts the binary" \
  "$(contains "$out" '^  example.com/viahelper/internal/rig: its tests import example.com/viahelper/internal/helper, which starts it (viahelper/internal/helper/helper.go:9)$')"
# The generated test main imports the package under test itself beside its external tests, so
# the shortest chain goes from the main straight to it.
check "names the shortest chain" \
  "$(contains "$out" 'The import chain: example.com/viahelper/internal/rig.test -> example.com/viahelper/internal/rig -> example.com/viahelper/internal/testpg -> github.com/testcontainers/testcontainers-go$')"

echo "case: a test file the default build leaves out is read under its one tag"
shapes "$work" tagged
gofile "$work/tagged/internal/rig/memory_test.go" '//go:build memory' '' 'package rig' '' \
  'import (' '	"os"' '	"os/exec"' '	"testing"' ')' '' \
  'func TestHarness(t *testing.T) { _ = exec.Command(os.Args[0]).Run(); _ = Start() }'
run_check tagged
check "exits 1" "$(is "$status" 1)"
check "names the build that starts itself" \
  "$(contains "$out" '^::error::tagged: the test binary of example.com/tagged/internal/rig built with -tags memory links testcontainers-go, and it starts itself (tagged/internal/rig/memory_test.go:11)')"

echo "case: a test file under any other constraint is refused, not skipped"
shapes "$work" compound
gofile "$work/compound/internal/client/memory_test.go" '//go:build memory && !race' '' 'package client' '' \
  'import (' '	"os"' '	"testing"' ')' '' 'func TestSelf(t *testing.T) { _ = os.Args[0] }'
run_check compound
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and the constraint it cannot read" \
  "$(contains "$out" "^::error::compound: compound/internal/client/memory_test.go:10 starts the test binary, and the default build leaves the file out under the constraint 'memory && !race', which is not one tag alone")"

echo "case: a main package in a testdata directory is a command too"
shapes "$work" standins
gofile "$work/standins/internal/rig/testdata/standin/standin.go" 'package main' '' \
  'import "example.com/standins/internal/rig"' '' 'func main() { println(rig.Start()) }'
# A generator a build constraint leaves out of every build is no binary, and no error either.
gofile "$work/standins/internal/client/testdata/gen/gen.go" '//go:build ignore' '' 'package main' '' 'func main() {}'
run_check standins
check "exits 1" "$(is "$status" 1)"
check "names the testdata command and its chain" \
  "$(contains "$out" '^::error::standins: the command example.com/standins/internal/rig/testdata/standin links testcontainers-go, and a command pays the init at every start. The import chain: example.com/standins/internal/rig/testdata/standin -> example.com/standins/internal/rig -> example.com/standins/internal/testpg -> github.com/testcontainers/testcontainers-go$')"
check "counts it beside cmd/seed, and not the excluded generator" \
  "$(contains "$out" '^standins: 1 of 2 binaries link no testcontainers-go (2 commands, 0 self-starting test binaries)$')"

echo "case: a module with no main package has not passed"
module "$work" nomain
gofile "$work/nomain/lib/lib.go" 'package lib'
run_check nomain
check "exits 1" "$(is "$status" 1)"
check "says nothing was checked" "$(contains "$out" '^::error::nomain: .*holds no main package, so this check saw no binary there: nothing was checked$')"

echo "case: a package go cannot list fails the check, with go's own reason"
shapes "$work" broken
gofile "$work/broken/internal/client/client.go" 'package client' '' 'import "example.com/broken/internal/missing"' '' \
  'func Ask() string { return missing.Value }'
run_check broken
check "exits 1" "$(is "$status" 1)"
check "says the closure is unknown" "$(contains "$out" '^::error::broken: .go list .*. failed (1), so what it lists is unknown:$')"
check "carries go's message" "$(contains "$out" 'example.com/broken/internal/missing')"

echo "case: a directory that is no module, and an option"
mkdir -p "$work/plain"
run_check plain
check "a plain directory exits 1" "$(is "$status" 1)"
check "a plain directory is named" "$(contains "$out" '^::error::plain is no Go module directory: it holds no go.mod$')"
run_check --list clean
check "an option exits 2" "$(is "$status" 2)"
check "an option is a usage error" "$(contains "$out" 'takes Go module directories and no option')"

echo "case: several modules are each judged, and one failing fails the run"
run_check clean command
check "exits 1" "$(is "$status" 1)"
check "passes the clean one" "$(contains "$out" '^clean: 2 of 2 binaries link no testcontainers-go')"
check "fails the other" "$(contains "$out" '^::error::command: the command example.com/command/cmd/seed links')"

summary "testcontainers_links.py"
