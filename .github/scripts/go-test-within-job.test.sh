#!/usr/bin/env bash
# Tests for `go-test-within-job.sh`: the -timeout it gives a go test is what is left of
# JOB_TIMEOUT_MINUTES since the runner's per-job process started, less 60 s; it fails when too
# little is left, and refuses a command, a limit or a process tree it cannot read, running nothing.
#
# Each case runs the real script under a stand-in for the runner's per-job process (bash under the
# name Runner.Worker, which the script reads the job's start off), with a stand-in `go` first on
# PATH that writes the arguments it was given and exits with a status the case picks, so no case
# compiles or runs a test.
#
# Run from anywhere: .github/scripts/go-test-within-job.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
wrapper="$script_dir/go-test-within-job.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"

# The stand-in go: its arguments, one per line, in go-args; its status is go-status's, 0 unless a
# case writes another.
cat > "$work/bin/go" <<GO
#!/usr/bin/env bash
printf '%s\n' "\$@" > "$work/go-args"
exit "\$(cat "$work/go-status" 2>/dev/null || echo 0)"
GO
chmod +x "$work/bin/go"
ln -s "$(type -P bash)" "$work/bin/Runner.Worker"

# drive [NAME=value...] -- <argument>...: the wrapper under the stand-in Runner.Worker, with
# JOB_TIMEOUT_MINUTES unset unless a pair sets it. Sets `out` and `status`. The `; exit $?` keeps
# bash from replacing itself with the wrapper, so Runner.Worker stays its parent.
drive() {
  local -a assignments=()
  while [ "$1" != -- ]; do
    assignments+=("$1")
    shift
  done
  shift
  rm -f "$work/go-args"
  set +e
  out=$(env -u JOB_TIMEOUT_MINUTES PATH="$work/bin:$PATH" "${assignments[@]}" \
    "$work/bin/Runner.Worker" -c '"$@"; exit $?' worker "$wrapper" "$@" 2>&1)
  status=$?
  set -e
}
ran_go() { [ -e "$work/go-args" ] && echo yes || echo no; }
go_args() { tr '\n' ' ' < "$work/go-args" | sed 's/ $//'; }

echo "case: a go test in a 32-minute job that has run 600 s"
# A stand-in ps first on PATH answers `-o etimes= -p <pid>` with 600 for the stand-in
# Runner.Worker alone (the process named Runner.Worker whose command line runs this harness's
# wrapper), and passes every other call to the real ps. So the -timeout is 1260 s only when the
# wrapper reads the elapsed time of the nearest Runner.Worker: one that read its own, or a real
# Runner.Worker above the harness (CI runs it under one), or read no time at all, gives another.
real_ps=$(type -P ps)
mkdir -p "$work/etimes600"
cat > "$work/etimes600/ps" <<PS
#!/usr/bin/env bash
if [ "\$#" = 4 ] && [ "\$1" = -o ] && [ "\$2" = etimes= ] && [ "\$3" = -p ] \\
  && [ "\$("$real_ps" -o comm= -p "\$4")" = Runner.Worker ] \\
  && [[ \$("$real_ps" -o args= -p "\$4") == *"$wrapper"* ]]; then
  echo 600
  exit 0
fi
exec "$real_ps" "\$@"
PS
chmod +x "$work/etimes600/ps"
drive JOB_TIMEOUT_MINUTES=32 PATH="$work/etimes600:$work/bin:$PATH" -- go test -count=1 -tags natslive -run '^(TestA|TestB)$' ./gcp
check "exits with the go test's status" "$(is "$status" 0)"
check "says the job has run 600 s and gives -timeout 1260s (32 min, less 600 s, less 60 s)" \
  "$(contains "$out" '^go-test-within-job.sh: the job has run 600 s of its 32 min; go test -timeout 1260s$')"
check "runs go test with the -timeout first and the rest as given" \
  "$(is "$(go_args)" "test -timeout 1260s -count=1 -tags natslive -run ^(TestA|TestB)\$ ./gcp")"

echo "case: the go test's own failure is the step's"
printf '1\n' > "$work/go-status"
drive JOB_TIMEOUT_MINUTES=32 -- go test ./gcp
rm -f "$work/go-status"
check "exits 1, go's status" "$(is "$status" 1)"

echo "case: a job whose limit leaves under 60 s"
# A 1-minute limit leaves at most 0 s after the 60 s spare however fast the stand-in is read.
drive JOB_TIMEOUT_MINUTES=1 -- go test ./gcp
check "fails with exit 1" "$(is "$status" 1)"
check "says too few seconds are left" "$(contains "$out" '^::error::go-test-within-job.sh: the job has run [0-9]* s of its 1 min, which leaves [0-9-]* s after 60 s for what follows the go test: too few to run it in$')"
check "and runs no go test" "$(is "$(ran_go)" no)"

echo "case: no JOB_TIMEOUT_MINUTES"
drive -- go test ./gcp
check "refuses with exit 2" "$(is "$status" 2)"
check "names the variable" "$(contains "$out" "JOB_TIMEOUT_MINUTES is '', not the job's timeout-minutes as a whole number")"
check "and runs no go test" "$(is "$(ran_go)" no)"

echo "case: a JOB_TIMEOUT_MINUTES that is not a whole number"
drive JOB_TIMEOUT_MINUTES=30m -- go test ./gcp
check "refuses with exit 2" "$(is "$status" 2)"
check "names what it read" "$(contains "$out" "JOB_TIMEOUT_MINUTES is '30m'")"

echo "case: a go test that sets its own -timeout"
drive JOB_TIMEOUT_MINUTES=32 -- go test -timeout 30m ./gcp
check "refuses with exit 2" "$(is "$status" 2)"
check "names the flag" "$(contains "$out" 'gives the go test its -timeout; drop `-timeout`')"
drive JOB_TIMEOUT_MINUTES=32 -- go test -timeout=30m ./gcp
check "and its = form" "$(contains "$out" 'drop `-timeout=30m`')"
check "and runs no go test" "$(is "$(ran_go)" no)"

echo "case: a command that is not go test"
drive JOB_TIMEOUT_MINUTES=32 -- go vet ./gcp
check "refuses with exit 2" "$(is "$status" 2)"
check "says it runs a go test" "$(contains "$out" 'runs a go test: give it `go test <flags and packages>`, not `go vet ./gcp`')"
check "and runs nothing" "$(is "$(ran_go)" no)"

echo "case: no Runner.Worker among its parents"
# CI runs this harness under a real Runner.Worker, which the walk would find, so this case runs the
# wrapper with a stand-in ps first on PATH that reads every process as a bash whose parent is 1.
mkdir -p "$work/no-worker"
cat > "$work/no-worker/ps" <<'PS'
#!/usr/bin/env bash
case " $* " in
  *" -o comm= "*) echo bash ;;
  *" -o ppid= "*) echo 1 ;;
  *) exit 2 ;;
esac
PS
chmod +x "$work/no-worker/ps"
rm -f "$work/go-args"
set +e
out=$(env PATH="$work/no-worker:$work/bin:$PATH" JOB_TIMEOUT_MINUTES=32 "$wrapper" go test ./gcp 2>&1)
status=$?
set -e
check "refuses with exit 2" "$(is "$status" 2)"
check "says the job's start cannot be read" "$(contains "$out" "no Runner.Worker among this process's parents")"
check "and runs no go test" "$(is "$(ran_go)" no)"

summary "go-test-within-job.sh"
