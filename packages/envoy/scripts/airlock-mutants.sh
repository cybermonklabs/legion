#!/usr/bin/env bash
# packages/envoy/scripts/airlock-mutants.sh
#
# Proves that the airlock's tests and the Lean proofs catch a defect, which is the whole claim
# behind a model that is only as good as the test tying it to the Go functions, and behind a
# proxy whose wire handling no model covers. Each file of internal/broker/airlock/testdata/mutants
# and of internal/broker/airlockproxy/testdata/mutants is one defect as a unified diff. For each,
# this script copies packages/envoy to a fresh directory under $TMPDIR, applies that one patch
# there with patch -p1, and requires the copy to fail:
#
#   - a Go mutant (go-*.patch) must fail the comparison fuzz test over the committed seed
#     corpus, run as `go test -run FuzzAirlock ./internal/broker/airlock/` with AIRLOCK_MODEL
#     naming the model this checkout already built;
#   - a model mutant (lean-*.patch) must fail `lake build` in the copy, because a proof no
#     longer holds, or, when it still builds, fail that same comparison against the mutated
#     model. The line says which of the two caught it;
#   - a proxy mutant (proxy-*.patch, internal/broker/airlockproxy/testdata/mutants) must fail the
#     tests its own header names, one `# must fail: <Test>` line each, run as
#     `go test -run '^(<Test>|...)$' ./internal/broker/airlockproxy/`, which runs each once with
#     an HTTP/1.1 client and once with an h2-only one. It is caught only when every named test ran
#     and failed: a failure of any other test is not counted, a named test that passed is a hole,
#     a named test that did not run (a misspelt or removed name) is refused, and a patch that names
#     no test is refused. These are the defects of what the model does not cover: how a request on
#     the wire, an HTTP/2 stream among them, reaches the decision, and what the proxy carries
#     after it.
#
# A mutant nothing catches is a hole in the tests or in the proofs, and this script fails on
# it. The patches are never applied to this repository: every one is applied inside the copy,
# and the run ends with jj's or git's status of the repository to show it is untouched.
#
# Usage: packages/envoy/scripts/airlock-mutants.sh [<name>...]
#        with no name, every mutant of both testdata/mutants directories; a name is a patch's
#        file name, with or without the .patch suffix (for example go-carries-value).
#
# Needs go, GNU patch or git (the patches are plain unified diffs, and `git apply -p1`
# applies them where patch is not installed), and, for the model mutants, lake from the Lean
# toolchain this environment names (ELAN_HOME and PATH are taken as they are); a missing
# lake exits 2. The Go mutants need the built model, and the proxy mutants need neither. One
# PASS: or FAIL: line per mutant, and a non-zero exit when any mutant was not caught.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$ENVOY_DIR/../.." && pwd)"
AIRLOCK_PKG_DIR="$ENVOY_DIR/internal/broker/airlock"
MUTANT_DIR="$AIRLOCK_PKG_DIR/testdata/mutants"
PROXY_MUTANT_DIR="$ENVOY_DIR/internal/broker/airlockproxy/testdata/mutants"
MODEL="$AIRLOCK_PKG_DIR/lean/.lake/build/bin/airlock-model"

usage() {
	cat <<'EOF'
usage: airlock-mutants.sh [<name>...]

Applies each mutant of internal/broker/airlock/testdata/mutants and of
internal/broker/airlockproxy/testdata/mutants to a copy of packages/envoy under $TMPDIR and
requires that copy to fail: a go-*.patch must fail the comparison fuzz test over the committed
seed corpus, a lean-*.patch must fail `lake build`, because a proof no longer holds, or that
same comparison against the mutated model, and a proxy-*.patch must fail every test of
./internal/broker/airlockproxy/ its own `# must fail: <Test>` lines name, and is counted by
those tests alone.

  <name>   a patch's file name, with or without the .patch suffix (for example
           go-carries-value or proxy-connect-carried); with no name, every mutant of both
           directories

No patch is ever applied to this repository. It prints one PASS: or FAIL: line per mutant and
a last line saying how many were caught, exits non-zero when a mutant was not caught, and
exits 2 when go, lake or both patch and git are missing.
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

if ! command -v go >/dev/null 2>&1; then
	echo "airlock-mutants: go is not on PATH" >&2
	exit 2
fi
if ! command -v patch >/dev/null 2>&1 && ! command -v git >/dev/null 2>&1; then
	echo "airlock-mutants: neither patch nor git is on PATH, so no mutant can be applied" >&2
	exit 2
fi

# apply_patch applies one mutant inside the copy, never in the repository. GNU patch is the
# tool the patches are written for; git apply reads the same unified diff, and is what this
# script uses where patch is not installed.
apply_patch() {
	local copy="$1" patch_file="$2" log="$3"
	if command -v patch >/dev/null 2>&1; then
		patch -p1 -s --batch --forward -d "$copy" -i "$patch_file" >"$log" 2>&1
	else
		(cd "$copy" && git apply -p1 "$patch_file") >"$log" 2>&1
	fi
}

# mutant_file is the patch a mutant's name names: a proxy-* mutant's under the proxy's own
# testdata, every other under the airlock's.
mutant_file() {
	case "$1" in
	proxy-*) echo "$PROXY_MUTANT_DIR/$1.patch" ;;
	*) echo "$MUTANT_DIR/$1.patch" ;;
	esac
}

# The mutants to run: the names given, else every patch of both testdata/mutants, in name order.
mutants=()
if [[ $# -gt 0 ]]; then
	for name in "$@"; do
		name="${name%.patch}"
		if [[ ! -f "$(mutant_file "$name")" ]]; then
			echo "airlock-mutants: no mutant named $name in $MUTANT_DIR or $PROXY_MUTANT_DIR" >&2
			exit 2
		fi
		mutants+=("$name")
	done
else
	for patch_file in "$MUTANT_DIR"/*.patch "$PROXY_MUTANT_DIR"/proxy-*.patch; do
		mutants+=("$(basename "$patch_file" .patch)")
	done
fi

needs_lake=false
needs_model=false
for name in "${mutants[@]}"; do
	case "$name" in
	lean-*) needs_lake=true ;;
	proxy-*) ;;
	*) needs_model=true ;;
	esac
done

if [[ "$needs_lake" == true ]] && ! command -v lake >/dev/null 2>&1; then
	echo "airlock-mutants: lake is not on PATH, so the model mutants cannot be built." >&2
	echo "  Put the Lean toolchain on PATH (ELAN_HOME/bin), or name only the go-* mutants." >&2
	exit 2
fi
if [[ "$needs_model" == true && ! -x "$MODEL" ]]; then
	echo "airlock-mutants: no built model at $MODEL" >&2
	echo "  Build it: (cd $AIRLOCK_PKG_DIR/lean && lake build)" >&2
	exit 2
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airlock-mutants.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

# copy_envoy writes a copy of packages/envoy into <dest>/packages/envoy, laid out as in the
# repository so the patches' own a/packages/envoy/... paths apply with patch -p1. A Go mutant
# takes no copy of the Lean build directory: it compares against the model this checkout
# already built, and .lake is the biggest thing in the tree.
copy_envoy() {
	local dest="$1" with_lake="$2"
	mkdir -p "$dest/packages"
	if [[ "$with_lake" == true ]]; then
		tar -C "$ENVOY_DIR" -cf - . | tar -C "$dest/packages" -xf - --one-top-level=envoy
	else
		tar -C "$ENVOY_DIR" -cf - --exclude=./internal/broker/airlock/lean/.lake . |
			tar -C "$dest/packages" -xf - --one-top-level=envoy
	fi
}

# compare runs the comparison fuzz test over the seed corpus in <copy>, against <model>, and
# answers 0 when the two sides disagreed, 1 when they agreed, 2 when the test did not run.
compare() {
	local copy="$1" model="$2" log="$3"
	if (cd "$copy/packages/envoy" && GOWORK=off AIRLOCK_MODEL="$model" \
		go test -count=1 -run FuzzAirlock ./internal/broker/airlock/) >"$log" 2>&1; then
		return 1
	fi
	# A non-zero exit is the mutant caught only when the comparison itself failed; a copy
	# that does not build or a model that does not answer is a broken run, not a catch.
	if grep -q -- '--- FAIL: FuzzAirlock' "$log"; then
		return 0
	fi
	return 2
}

# failing_seed names the first corpus seed the comparison failed on, for the PASS: line.
failing_seed() {
	sed -n 's|^ *--- FAIL: FuzzAirlock/\([^ ]*\).*|\1|p' "$1" | head -n 1
}

# must_fail is the tests a proxy mutant's header names, one `# must fail: <Test>` line each, one
# per output line.
must_fail() {
	sed -n 's|^# must fail: \(Test[A-Za-z0-9_]*\)[[:space:]]*$|\1|p' "$1"
}

# proxy_tests runs <tests>, the named tests of a proxy mutant, in <copy> and answers 0 when every
# one of them ran and failed, 1 when one of them passed, 2 when one of them did not run - a copy
# that does not build, or a name no test of the suite has. MISSED is set to the ones that passed or
# did not run.
proxy_tests() {
	local copy="$1" log="$2" tests="$3" test verdict=0
	(cd "$copy/packages/envoy" && GOWORK=off go test -count=1 -v -run "^($(paste -sd '|' <<<"$tests"))\$" ./internal/broker/airlockproxy/) >"$log" 2>&1
	MISSED=""
	while read -r test; do
		if ! grep -q "^=== RUN   $test\$" "$log"; then
			MISSED="$MISSED $test (did not run)"
			verdict=2
		elif ! grep -q "^--- FAIL: $test " "$log"; then
			MISSED="$MISSED $test (passed)"
			[ "$verdict" = 2 ] || verdict=1
		fi
	done <<<"$tests"
	return "$verdict"
}

failures=0
for name in "${mutants[@]}"; do
	started=$SECONDS
	copy="$WORK_DIR/$name"
	log="$WORK_DIR/$name.log"
	case "$name" in
	lean-*) copy_envoy "$copy" true ;;
	*) copy_envoy "$copy" false ;;
	esac
	if ! apply_patch "$copy" "$(mutant_file "$name")" "$log"; then
		echo "FAIL: $name — the patch does not apply: $(head -n 3 "$log" | tr '\n' ' ')"
		failures=$((failures + 1))
		continue
	fi

	if [[ "$name" == proxy-* ]]; then
		tests="$(must_fail "$(mutant_file "$name")")"
		if [ -z "$tests" ]; then
			echo "FAIL: $name — the patch names no test that must fail (a '# must fail: <Test>' line), so nothing it could catch would count"
			failures=$((failures + 1))
			continue
		fi
		set +e
		proxy_tests "$copy" "$log" "$tests"
		verdict=$?
		set -e
		case "$verdict" in
		0) echo "PASS: $name — caught by each test it names: $(tr '\n' ' ' <<<"$tests")($((SECONDS - started))s)" ;;
		1)
			echo "FAIL: $name — not caught by every test it names:$MISSED ($((SECONDS - started))s)"
			failures=$((failures + 1))
			;;
		*)
			echo "FAIL: $name — a test it names did not run:$MISSED; $(tail -n 3 "$log" | tr '\n' ' ')"
			failures=$((failures + 1))
			;;
		esac
		continue
	fi

	if [[ "$name" == lean-* ]]; then
		if ! (cd "$copy/packages/envoy/internal/broker/airlock/lean" && lake build) >"$log" 2>&1; then
			reason="$(grep -m1 -E 'error:' "$log" | cut -c1-120)"
			echo "PASS: $name — caught by lake build, a proof no longer holds: ${reason:-see the build log} ($((SECONDS - started))s)"
			continue
		fi
		model="$copy/packages/envoy/internal/broker/airlock/lean/.lake/build/bin/airlock-model"
	else
		model="$MODEL"
	fi

	set +e
	compare "$copy" "$model" "$log"
	verdict=$?
	set -e
	case "$verdict" in
	0) echo "PASS: $name — caught by the comparison fuzz test on seed $(failing_seed "$log") ($((SECONDS - started))s)" ;;
	1)
		echo "FAIL: $name — nothing caught it: the mutated code still agrees with the model ($((SECONDS - started))s)"
		failures=$((failures + 1))
		;;
	*)
		echo "FAIL: $name — the comparison did not run: $(tail -n 3 "$log" | tr '\n' ' ')"
		failures=$((failures + 1))
		;;
	esac
done

echo
if [[ $failures -gt 0 ]]; then
	echo "$failures of ${#mutants[@]} mutants were not caught"
else
	echo "${#mutants[@]} mutants, each caught"
fi

# Last, the assurance that this run changed nothing here: every patch was applied inside
# WORK_DIR, which the EXIT trap removes, and never in the repository. jj's status where the
# checkout is a jj repository and git's where it is a git one, such as a git worktree; neither
# decides the exit, which is the mutants' alone.
if command -v jj >/dev/null 2>&1 && jj -R "$REPO_DIR" root >/dev/null 2>&1; then
	echo "The repository itself was never patched; jj -R $REPO_DIR status --no-pager:"
	jj -R "$REPO_DIR" status --no-pager || true
elif git -C "$REPO_DIR" rev-parse --git-dir >/dev/null 2>&1; then
	echo "The repository itself was never patched; git -C $REPO_DIR status --short:"
	git -C "$REPO_DIR" status --short || true
else
	echo "The repository itself was never patched: every patch was applied under $WORK_DIR, never in $REPO_DIR."
fi

[[ $failures -eq 0 ]]
