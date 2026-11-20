#!/usr/bin/env bash
# Tests for `private_proc.py`: every route a test reads the processes by sees its own namespace's
# alone, which the same test run without the script does not, whichever way the namespace is made
# (an unprivileged user namespace, or sudo -n with --sudo or where the kernel refuses the first);
# the test binary runs beneath a supervisor at PID 1, as its own user, in its own directory, with
# its environment; the script lists the tests that read a process table, each with the root it
# read, from the record each binary that links its module's internal/hostproc writes; and it says
# UNKNOWN, and fails, for a run in which no test binary ran in a namespace of its own, a list with
# no test in it, a package with tests that ran in none while the others did, a binary that links
# internal/hostproc and recorded nothing, and a scan whose test the record could not name. It also
# fails a namespace it could not make, refuses a machine where neither way makes one (naming both
# reasons) and --sudo where sudo -n does not answer (never falling back), and fails a supervisor
# that finds itself in no namespace of its own, and a command that is no go test or that sets -exec
# itself.
#
# The cases run a real `go test` over two scratch modules written here. The first's test reads
# /proc by five routes: a direct directory read, os.DirFS, a joined glob, an opened directory
# handed to a helper, and pgrep. Each must find a process the test started and must not find one
# this harness started on the machine before the run; the two are told apart by a marker in their
# command line, never by a pid, which two namespaces can share. The second module's
# internal/hostproc stands in for packages/daemon's: it writes the record's lines as that one does
# (hostproc.RecordEnv), and names a scan by its test only in the binary the wrapper marks as the one
# go ran (hostproc.BinaryEnv), so the list and its UNKNOWNs are read from a binary that links it, one
# that links it and scans nothing, and one that does not link it. Stand-ins for unshare and sudo, first
# on PATH, make each way fail where a case needs it to. The script needs both ways to work here: a
# kernel that lets this user make a user namespace (ci-job-test.yaml lifts the runner's AppArmor
# restriction before it runs this), and `sudo -n`, which CI's runners give; and a go on PATH.
#
# Run from anywhere: .github/scripts/private_proc.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
wrapper="$script_dir/private_proc.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
outside=""
cleanup() {
  if [ -n "$outside" ]; then kill "$outside" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

if ! said=$("$wrapper" --check 2>&1); then
  echo "private_proc.test.sh needs a kernel that lets this user make a user namespace, the wrapper's first way: $said" >&2
  exit 1
fi
# The sudo route's cases run where the wrapper's sudo -n unshare makes a namespace. Elsewhere each
# is printed NOT RUN by name, never counted a pass, and the harness ends UNKNOWN (exit 2); CI's
# runner gives sudo -n, so there it is no reason to leave a case out, and the harness fails.
sudo_said=""
if ! said=$("$wrapper" --check --sudo 2>&1); then
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    echo "private_proc.test.sh needs sudo -n on CI's runner, the wrapper's second way: $said" >&2
    exit 1
  fi
  sudo_said=${said#private-proc: }
fi
NOT_RUN=0
# sudo_case <case>: true where the sudo route can run here; else the case is NOT RUN, by name.
sudo_case() {
  echo "case: $1"
  [ -z "$sudo_said" ] && return 0
  echo "  NOT RUN: $1: the wrapper's sudo -n unshare makes no namespace here ($sudo_said)"
  NOT_RUN=$((NOT_RUN + 1))
  return 1
}

# The scratch module: one package with the routes' test, and one with no test at all.
module="$work/routes"
mkdir -p "$module/notests"
printf 'module routes\n\ngo 1.22\n' > "$module/go.mod"
printf 'package notests\n\n// Nothing here is tested.\nconst Nothing = 0\n' > "$module/notests/notests.go"
cat > "$module/routes_test.go" <<'GO'
package routes

import (
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cmdlines is the command line of every process names lists, read under root by read.
func cmdlines(names []string, read func(string) ([]byte, error)) []string {
	var out []string
	for _, name := range names {
		if _, err := strconv.Atoi(name); err != nil {
			continue
		}
		if body, err := read(filepath.Join(name, "cmdline")); err == nil {
			out = append(out, strings.ReplaceAll(string(body), "\x00", " "))
		}
	}
	return out
}

// namesOf is the helper an opened directory is handed to.
func namesOf(dir *os.File) []string {
	names, _ := dir.Readdirnames(-1)
	return names
}

func TestEveryRouteSeesThisNamespaceAlone(t *testing.T) {
	if os.Getenv("ROUTES_FAIL") == "1" {
		t.Fatal("the harness asked this run to fail")
	}
	outside := os.Getenv("ROUTES_OUTSIDE_MARKER")
	if outside == "" {
		t.Fatal("ROUTES_OUTSIDE_MARKER names no marker of a process outside")
	}
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	inside := "inside-marker-" + hex.EncodeToString(random)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	child := &exec.Cmd{Path: sleep, Args: []string{inside, "60"}}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child.Process.Pid), "cmdline")); err == nil && strings.Contains(string(body), inside) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the inside marker's command line never showed")
		}
	}
	rooted := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("/proc", name)) }
	routes := map[string]func() []string{
		"a direct directory read": func() []string {
			entries, _ := os.ReadDir("/proc")
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return cmdlines(names, rooted)
		},
		"os.DirFS": func() []string {
			fsys := os.DirFS("/proc")
			entries, _ := fs.ReadDir(fsys, ".")
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return cmdlines(names, func(name string) ([]byte, error) { return fs.ReadFile(fsys, name) })
		},
		"a joined glob": func() []string {
			paths, _ := filepath.Glob(filepath.Join("/proc", "[0-9]*", "cmdline"))
			var out []string
			for _, path := range paths {
				if body, err := os.ReadFile(path); err == nil {
					out = append(out, strings.ReplaceAll(string(body), "\x00", " "))
				}
			}
			return out
		},
		"an opened directory handed to a helper": func() []string {
			dir, err := os.Open("/proc")
			if err != nil {
				return nil
			}
			defer dir.Close()
			return cmdlines(namesOf(dir), rooted)
		},
		"pgrep": func() []string {
			out, _ := exec.Command("pgrep", "-a", "-f", ".").Output()
			return strings.Split(string(out), "\n")
		},
	}
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		seen := strings.Join(routes[name](), "\n")
		if !strings.Contains(seen, inside) {
			t.Errorf("%s does not find the inside marker, a process this test started", name)
		}
		if strings.Contains(seen, outside) {
			t.Errorf("%s finds the outside marker, a process of the machine this test did not start", name)
		}
	}
}

func TestTheBinaryRunsBeneathASupervisor(t *testing.T) {
	if pid := os.Getpid(); pid == 1 {
		t.Errorf("the test binary is PID 1, want a supervisor there")
	}
	if parent := os.Getppid(); parent != 1 {
		t.Errorf("the test binary's parent is pid %d, want the supervisor at PID 1", parent)
	}
	if uid := strconv.Itoa(os.Getuid()); uid != os.Getenv("ROUTES_UID") {
		t.Errorf("the test binary runs as uid %s, want the user's %s", uid, os.Getenv("ROUTES_UID"))
	}
	groups, _ := os.Getgroups()
	slices.Sort(groups)
	var got []string
	for _, group := range groups {
		got = append(got, strconv.Itoa(group))
	}
	if strings.Join(got, ",") != os.Getenv("ROUTES_GROUPS") {
		t.Errorf("the test binary holds groups %v, want the user's %s", got, os.Getenv("ROUTES_GROUPS"))
	}
	if dir, _ := os.Getwd(); dir != os.Getenv("ROUTES_DIR") {
		t.Errorf("the test binary runs in %s, want its package's directory %s", dir, os.Getenv("ROUTES_DIR"))
	}
}
GO

marker="outside-marker-$$-$RANDOM$RANDOM"
bash -c "exec -a $marker sleep 600" > /dev/null 2>&1 &
outside=$!
for _ in $(seq 50); do
  if tr '\0' ' ' < "/proc/$outside/cmdline" 2>/dev/null | grep -q -- "$marker"; then break; fi
  sleep 0.1
done
uid=$(id -u)
gid=$(id -g)
# The groups the binary holds: through sudo, the user's own; in a user namespace the user's own
# too, of which the namespace maps the user's gid alone, so each other reads as the overflow group.
groups=$(id -G | tr ' ' '\n' | sort -n | paste -sd,)
overflow=$(</proc/sys/kernel/overflowgid)
groups_in_user_namespace=$(id -G | tr ' ' '\n' | awk -v gid="$gid" -v over="$overflow" '{print ($1 == gid ? $1 : over)}' | sort -n | paste -sd,)

# run <command...>: runs it in the scratch module with the routes' environment, and leaves its
# merged output in $out and its status in $status. The binary's groups are wanted as the user
# namespace shows them, or, with want_groups set, as that says.
run() {
  set +e
  out=$(cd "$module" && env ROUTES_OUTSIDE_MARKER="$marker" ROUTES_UID="$uid" \
    ROUTES_GROUPS="${want_groups:-$groups_in_user_namespace}" ROUTES_DIR="$(cd "$module" && pwd -P)" "$@" 2>&1)
  status=$?
  set -e
}
said_user="private-proc: each test binary's namespace is made with an unprivileged user namespace (unshare --user --map-root-user)"
said_sudo="private-proc: each test binary's namespace is made with sudo -n unshare, as --sudo asks"
one_ran='private-proc: 1 test binaries ran, each beneath PID 1 of a PID namespace of its own, of the 1 packages with tests'

# Stand-ins first on PATH: an unshare that refuses a user namespace, as a kernel that restricts one
# does, and passes every other call to the real one; an unshare that makes the probe's namespace
# and no binary's; and a sudo that asks for a password, writing each call's words to its log first.
real_unshare=$(command -v unshare)
mkdir -p "$work/no-userns" "$work/probe-only" "$work/no-sudo" "$work/tmp"
cat > "$work/no-userns/unshare" <<SH
#!/usr/bin/env bash
for word in "\$@"; do
  if [ "\$word" = --user ]; then echo "unshare: unshare failed: Operation not permitted" >&2; exit 1; fi
done
exec $real_unshare "\$@"
SH
cat > "$work/probe-only/unshare" <<SH
#!/usr/bin/env bash
for word in "\$@"; do
  if [ "\$word" = --init ]; then echo "unshare: cannot open /proc/self/uid_map: Permission denied" >&2; exit 1; fi
done
exec $real_unshare "\$@"
SH
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "%s/sudo-calls"\necho "sudo: a password is required" >&2\nexit 1\n' "$work" > "$work/no-sudo/sudo"
chmod +x "$work/no-userns/unshare" "$work/probe-only/unshare" "$work/no-sudo/sudo"

echo "case: every route sees the namespace's processes and none of the machine's, in a user namespace"
run "$wrapper" go test -count=1 -v ./...
check "exits 0" "$(is "$status" 0)"
check "says it makes each namespace as a user namespace" "$(contains "$out" "^$said_user\$")"
check "the routes' test passes" "$(contains "$out" '^--- PASS: TestEveryRouteSeesThisNamespaceAlone')"
check "the supervisor's test passes" "$(contains "$out" '^--- PASS: TestTheBinaryRunsBeneathASupervisor')"
check "says the one binary ran beneath PID 1 of a namespace of its own" "$(contains "$out" "$one_ran")"

if sudo_case "with --sudo, every route sees the namespace's processes alone, through sudo -n unshare"; then
  want_groups=$groups run "$wrapper" --sudo go test -count=1 -v ./...
  check "exits 0" "$(is "$status" 0)"
  check "says it makes each namespace with sudo, as --sudo asks" "$(contains "$out" "^$said_sudo\$")"
  check "the routes' test passes" "$(contains "$out" '^--- PASS: TestEveryRouteSeesThisNamespaceAlone')"
  check "the supervisor's test passes, the binary holding the user's groups" "$(contains "$out" '^--- PASS: TestTheBinaryRunsBeneathASupervisor')"
  check "says the one binary ran beneath PID 1 of a namespace of its own" "$(contains "$out" "$one_ran")"
fi

if sudo_case "where the kernel refuses a user namespace, the namespace is made with sudo -n unshare"; then
  want_groups=$groups run env PATH="$work/no-userns:$PATH" "$wrapper" go test -count=1 -v ./...
  check "exits 0" "$(is "$status" 0)"
  check "says it takes sudo, and why" \
    "$(contains "$out" "made with sudo -n unshare, since this machine refuses the first way: an unprivileged user namespace (unshare --user --map-root-user): unshare: unshare failed: Operation not permitted")"
  check "the routes' test passes" "$(contains "$out" '^--- PASS: TestEveryRouteSeesThisNamespaceAlone')"
  check "the supervisor's test passes" "$(contains "$out" '^--- PASS: TestTheBinaryRunsBeneathASupervisor')"
fi

echo "case: where neither way can make a namespace, the command is refused naming both, and no test runs"
run env PATH="$work/no-userns:$work/no-sudo:$PATH" "$wrapper" go test -count=1 -v ./...
check "exits 1" "$(is "$status" 1)"
check "names both reasons" "$(contains "$out" 'neither way can make one here: an unprivileged user namespace (unshare --user --map-root-user): unshare: unshare failed: Operation not permitted; sudo -n unshare: sudo: a password is required')"
check "runs no test" "$(is "$(contains "$out" 'TestEveryRouteSeesThisNamespaceAlone')" false)"
run env PATH="$work/no-userns:$work/no-sudo:$PATH" "$wrapper" --check
check "--check exits 1 and names both" "$(is "$status/$(contains "$out" 'neither way can make one here')" 1/true)"

echo "case: --sudo where sudo -n does not answer is refused by name, and never falls back to a user namespace"
: > "$work/sudo-calls"
run env PATH="$work/no-sudo:$PATH" "$wrapper" --sudo go test -count=1 -v ./...
check "exits 1" "$(is "$status" 1)"
check "names sudo's reason" "$(contains "$out" 'sudo takes sudo -n unshare alone, which cannot make one here: sudo -n unshare: sudo: a password is required')"
check "runs no test, and takes no user namespace" "$(is "$(contains "$out" 'TestEveryRouteSeesThisNamespaceAlone\|unprivileged user namespace')" false)"
check "asked sudo for exactly sudo -n unshare's probe, once" \
  "$(is "$(grep -c -- "^-n unshare --pid --fork --mount-proc --kill-child -- [^ ]*python[^ ]* $(realpath "$wrapper") --probe sudo $uid $gid " "$work/sudo-calls")/$(wc -l < "$work/sudo-calls")" 1/1)"

echo "case: the control, the same test without the script, sees the machine"
run go test -count=1 ./...
check "exits non-zero" "$(is "$([ "$status" -ne 0 ] && echo yes)" yes)"
for route in 'a direct directory read' 'os.DirFS' 'a joined glob' 'an opened directory handed to a helper' 'pgrep'; do
  check "$route finds the outside marker" "$(contains "$out" "$route finds the outside marker")"
done
check "the binary's parent is no supervisor" "$(contains "$out" "the test binary's parent is pid")"

echo "case: a failing test's status passes through"
run env ROUTES_FAIL=1 "$wrapper" go test -count=1 ./...
check "exits 1" "$(is "$status" 1)"
check "go names the failure" "$(contains "$out" 'the harness asked this run to fail')"

if sudo_case "a record in a sticky directory others can write, as a runner's /tmp, still takes the binary through sudo"; then
  # Through sudo the supervisor is root and the record is the user's; where fs.protected_regular is
  # set, Linux refuses root an O_CREAT open of another user's file in such a directory, which a
  # runner's TMPDIR is and this machine's may not be.
  want_groups=$groups run env TMPDIR=/tmp "$wrapper" --sudo go test -count=1 ./...
  check "exits 0" "$(is "$status" 0)"
  check "says the one binary ran beneath PID 1 of a namespace of its own" "$(contains "$out" "$one_ran")"
  check "the supervisor wrote its record" "$(is "$(printf '%s\n' "$out" | grep -c 'PermissionError')" 0)"
fi

echo "case: a go test that ran no test binary says UNKNOWN and fails"
# go test -c compiles the binary and runs nothing, and exits 0: the record then holds no package,
# which is what the script must catch. (go never answers a run from its cache while -exec is set.)
run "$wrapper" go test -c -o "$work/routes.test" .
check "exits 1" "$(is "$status" 1)"
check "the binary was built" "$(is "$([ -x "$work/routes.test" ] && echo built)" built)"
check "says UNKNOWN: nothing ran" "$(contains "$out" 'private-proc: UNKNOWN: no test binary ran in a namespace of its own, so nothing was read')"
check "prints no list" "$(is "$(contains "$out" 'the tests that read a process table')" false)"

echo "case: a list with no test in it says UNKNOWN and fails"
run "$wrapper" go test -count=1 ./notests/
check "exits 1" "$(is "$status" 1)"
check "says UNKNOWN: the list runs nothing" "$(contains "$out" 'private-proc: UNKNOWN: no package of ./notests/ has a test file, so no test ran and nothing was read')"

echo "case: a namespace that cannot be made fails the binary, and leaves no environment behind"
# The stand-in makes the probe's namespace, so the command takes the user namespace, and then
# refuses the binary's.
run env PATH="$work/probe-only:$PATH" TMPDIR="$work/tmp" "$wrapper" go test -count=1 ./...
check "exits non-zero" "$(is "$([ "$status" -ne 0 ] && echo yes)" yes)"
check "says the namespace was not made" "$(contains "$out" 'was not made (unshare exited 1); nothing ran on the machine')"
check "no file of the environment is left" "$(is "$(find "$work/tmp" -name 'private-proc-env-*' | wc -l)" 0)"

echo "case: a supervisor that finds itself in no namespace of its own refuses"
printf 'X=1' > "$work/env"
for route in user sudo; do
  set +e
  out=$("$wrapper" --init "$route" "$work/env" "$uid" "$gid" "$groups" "$work" "$work/record" -- /bin/true 2>&1)
  status=$?
  set -e
  check "exits 125 ($route)" "$(is "$status" 125)"
  check "says it is no PID 1 ($route)" "$(contains "$out" 'no namespace of its own for /bin/true: the supervisor is pid')"
  check "starts nothing, and records nothing ($route)" "$(is "$([ -e "$work/record" ] && echo recorded || echo none)" none)"
  printf 'X=1' > "$work/env"
done

echo "case: a command that is no go test, or that sets -exec, is refused"
run "$wrapper" go vet ./...
check "go vet exits 2" "$(is "$status" 2)"
run "$wrapper" go test -exec /bin/env ./...
check "a -exec of its own exits 2" "$(is "$status" 2)"
check "says why" "$(contains "$out" 'sets -exec itself, which would run its test binaries outside a namespace of their own')"
run "$wrapper" go test --exec=/bin/env ./...
check "a --exec= of its own exits 2 too, as go reads it for -exec" "$(is "$status" 2)"

# The second module: a stand-in for packages/daemon's internal/hostproc, which writes the record's
# lines as that package does, and three packages with tests: reads, whose test records three scans
# of its own, a root of its own, a reason's and its namespace's /proc (and, with SCANS_REFUSE=1, a
# refusal, and ends its binary as a refusal does; with SCANS_UNNAMED=1, a scan whose test the
# record could not name); quiet, which links the stand-in and scans nothing; and plain, which does
# not link it.
scans="$work/scans"
mkdir -p "$scans/internal/hostproc" "$scans/reads" "$scans/quiet" "$scans/plain"
printf 'module scans\n\ngo 1.22\n' > "$scans/go.mod"
cat > "$scans/internal/hostproc/hostproc.go" <<'GO'
// Package hostproc stands in for packages/daemon's internal/hostproc: it writes the lines of the
// record HOSTPROC_RECORD names as that package does, a binary line as the binary starts and a
// scan line for each Scan, and nothing at all with SCANS_SILENT=1. As that package does, it takes
// HOSTPROC_BINARY, which the wrapper sets for the binary go ran alone, out of its environment as it
// starts, and a process that had none is a child, whose scans are named by HOSTPROC_TEST alone: so
// a wrapper that no longer marks the binary makes every scan of the binary's own unnamed.
package hostproc

import (
	"os"
	"strconv"
	"strings"
)

func write(fields ...string) {
	path := os.Getenv("HOSTPROC_RECORD")
	if path == "" || os.Getenv("SCANS_SILENT") == "1" {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.Join(fields, "\t") + "\n"); err != nil {
		panic(err)
	}
}

// binary is whether this process is the binary go ran.
var binary bool

func init() {
	_, binary = os.LookupEnv("HOSTPROC_BINARY")
	_ = os.Unsetenv("HOSTPROC_BINARY")
	write("binary", strconv.Itoa(os.Getpid()))
}

// Scan records one scan of test's, or, in a child, of the test HOSTPROC_TEST names.
func Scan(kind, test, scanner, root, reason string) {
	if !binary {
		test = os.Getenv("HOSTPROC_TEST")
	}
	write("scan", kind, test, scanner, root, reason)
}
GO
cat > "$scans/reads/reads_test.go" <<'GO'
package reads

import (
	"os"
	"testing"

	"scans/internal/hostproc"
)

func TestReadsARootOfItsOwn(t *testing.T) {
	hostproc.Scan("own", "TestReadsARootOfItsOwn", "the stand-in's scanner", "/fixture/proc", "")
	hostproc.Scan("reason", "TestReadsARootOfItsOwn", "the reasoned stand-in", "/proc", "it takes only its own pid")
	hostproc.Scan("namespace", "TestReadsARootOfItsOwn", "the namespace stand-in", "/proc", "")
	if os.Getenv("SCANS_UNNAMED") == "1" {
		hostproc.Scan("own", "", "the unnamed stand-in", "/fixture/proc", "")
	}
	if os.Getenv("SCANS_REFUSE") == "1" {
		hostproc.Scan("refused", "TestReadsARootOfItsOwn", "the refused stand-in", "/proc", "")
		os.Exit(86)
	}
}
GO
printf 'package quiet\n\nimport (\n\t"testing"\n\n\t_ "scans/internal/hostproc"\n)\n\nfunc TestScansNothing(t *testing.T) {}\n' > "$scans/quiet/quiet_test.go"
printf 'package plain\n\nimport "testing"\n\nfunc TestLinksNoScanner(t *testing.T) {}\n' > "$scans/plain/plain_test.go"

# run_scans <command...>: run, in the second module.
run_scans() {
  set +e
  out=$(cd "$scans" && "$@" 2>&1)
  status=$?
  set -e
}

echo "case: the list names each test that read a process table, with the root it read, and no UNKNOWN"
run_scans "$wrapper" go test -count=1 ./...
check "exits 0" "$(is "$status" 0)"
check "names the package whose test read one" "$(contains "$out" '^private-proc:   reads: 1 tests read a process table$')"
check "names its own root" "$(contains "$out" "^private-proc:     TestReadsARootOfItsOwn: .*the stand-in's scanner, own /fixture/proc")"
check "names the host's, with its reason" "$(contains "$out" "^private-proc:     TestReadsARootOfItsOwn: .*the reasoned stand-in, host /proc, by its reason: it takes only its own pid")"
check "names the namespace's /proc as the test's own" "$(contains "$out" "^private-proc:     TestReadsARootOfItsOwn: .*the namespace stand-in, own /proc (the /proc of a PID namespace whose PID 1 is the test's process)")"
check "says the package that scanned nothing read none" "$(contains "$out" '^private-proc:   quiet: no test read a process table$')"
check "says the package with no scanner has none" "$(contains "$out" '^private-proc:   plain: no scanner of the module is in its test binary (it does not link internal/hostproc)$')"
check "says no UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"
check "says the three binaries ran" "$(contains "$out" 'private-proc: 3 test binaries ran, each beneath PID 1 of a PID namespace of its own, of the 3 packages with tests')"

echo "case: a refusal is named in the list, and the binary's failure is the step's"
run_scans env SCANS_REFUSE=1 "$wrapper" go test -count=1 ./...
check "exits 1, go's own status" "$(is "$status" 1)"
check "names the refusal and the binary's exit" "$(contains "$out" "refused: the refused stand-in, host /proc, with no reason (the package's binary exited 86)")"

echo "case: binaries that link internal/hostproc and recorded nothing say UNKNOWN and fail"
run_scans env SCANS_SILENT=1 "$wrapper" go test -count=1 ./...
check "exits 1" "$(is "$status" 1)"
check "names both binaries" "$(contains "$out" 'private-proc: UNKNOWN: the test binary of quiet, reads links internal/hostproc and recorded nothing, so what its tests read is not known')"
check "lists neither as a package that read none" "$(is "$(contains "$out" '^private-proc:   \(quiet\|reads\): no test read a process table$')" false)"
check "prints no success line" "$(is "$(contains "$out" 'test binaries ran, each beneath PID 1')" false)"

echo "case: a scan whose test the record could not name says UNKNOWN, naming the package, and fails"
run_scans env SCANS_UNNAMED=1 "$wrapper" go test -count=1 ./...
check "exits 1" "$(is "$status" 1)"
check "lists the package's named test as before" "$(contains "$out" "^private-proc:     TestReadsARootOfItsOwn: .*the stand-in's scanner, own /fixture/proc")"
check "counts the scan whose test is not known under the package" "$(contains "$out" '^private-proc:   reads: 1 scans whose test is not known, said under UNKNOWN below$')"
check "names the package and the scan under UNKNOWN" "$(contains "$out" "^private-proc: UNKNOWN: reads made 1 scans whose test its record could not name .*: the unnamed stand-in, own /fixture/proc$")"
check "puts the scan under no test" "$(is "$(contains "$out" '^private-proc:     [^ ]*: .*the unnamed stand-in')" false)"
check "prints no success line" "$(is "$(contains "$out" 'test binaries ran, each beneath PID 1')" false)"

echo "case: a package that ran outside the namespace while the others ran in theirs says UNKNOWN and fails"
# A stand-in for unshare runs plain's binary on the machine, outside the namespace and so outside
# the supervisor's record, and removes the environment file as the supervisor would, so its wrapper
# takes it for a binary that ran; every other call, the probe's among them, goes to the real
# unshare. go exits 0.
mkdir -p "$work/escape"
cat > "$work/escape/unshare" <<SH
#!/usr/bin/env bash
args=("\$@")
at=
for i in "\${!args[@]}"; do [ "\${args[\$i]}" = --init ] && at=\$i; done
if [ -n "\$at" ] && [ "\${args[at+6]##*/}" = plain ]; then
  rm -f "\${args[at+2]}"
  cd "\${args[at+6]}" && exec "\${args[@]:at+9}"
fi
exec $real_unshare "\$@"
SH
chmod +x "$work/escape/unshare"
run_scans env PATH="$work/escape:$PATH" "$wrapper" go test -count=1 ./...
check "exits 1" "$(is "$status" 1)"
check "names the package that ran in no namespace" "$(contains "$out" 'private-proc: UNKNOWN: 1 of the 3 packages with tests ran in no namespace of their own, so what their tests read is not known: plain')"
check "prints no success line" "$(is "$(contains "$out" 'test binaries ran, each beneath PID 1')" false)"

if [ "$NOT_RUN" -gt 0 ]; then
  echo
  echo "---"
  echo "Results: $PASS passed, $FAIL failed, $NOT_RUN cases not run"
  [ "$FAIL" -eq 0 ] || exit 1
  echo "UNKNOWN: $NOT_RUN cases of the sudo route did not run here (each NOT RUN above); CI's test job runs them"
  exit 2
fi
summary "private_proc.py"
