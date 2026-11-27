#!/usr/bin/env bash
# packages/envoy/scripts/airlock-mutants.sh
#
# Proves that the airlock's tests and the Lean proofs catch a defect, which is the whole claim
# behind a model that is only as good as the test tying it to the Go functions, and behind a
# proxy whose wire handling no model covers. Each file of internal/cipher/proxyrules/testdata/mutants,
# of internal/cipher/proxy/testdata/mutants, of internal/cipher/policy/testdata/mutants and of
# internal/cipher/requests/testdata/mutants is one defect as a unified diff. For each,
# this script copies packages/envoy to a fresh directory under $TMPDIR, applies that one patch
# there with patch -p1, and requires the copy to fail:
#
#   - a Go mutant (go-*.patch) must fail the comparison fuzz test over the committed seed
#     corpus, run as `go test -run FuzzAirlock ./internal/cipher/proxyrules/` with AIRLOCK_MODEL
#     naming the model this checkout already built;
#   - a model mutant (lean-*.patch) must fail `lake build` in the copy, because a proof no
#     longer holds, or, when it still builds, fail that same comparison against the mutated
#     model. The line says which of the two caught it, and for the build, the theorems of
#     Airlock/Proofs.lean its errors are in. A build that fails in another file (the model or
#     the program around it) is a mutant that does not build, and fails the run rather than
#     counting as caught;
#   - a proxy mutant (proxy-*.patch, internal/cipher/proxy/testdata/mutants) must fail the
#     tests its own header names, one `# must fail: <Test>` line each, run as
#     `go test -run '^(<Test>|...)$' ./internal/cipher/proxy/`, which runs each once with
#     an HTTP/1.1 client and once with an h2-only one. It is caught only when every named test ran
#     and failed: a failure of any other test is not counted, a named test that passed is a hole,
#     a named test that did not run (a misspelt or removed name) is refused, and a patch that names
#     no test is refused. These are the defects of what the model does not cover: how a request on
#     the wire, an HTTP/2 stream among them, reaches the decision, and what the proxy carries
#     after it.
#   - a policy mutant (policy-*.patch, internal/cipher/policy/testdata/mutants) is held to the same
#     rule against `go test -run '^(<Test>|...)$' ./internal/cipher/policy/`: every test its
#     `# must fail: <Test>` lines name must run and fail. These are defects of what the policy
#     load fingerprints, which a live grant is held to at its release.
#   - a requests mutant (requests-*.patch, internal/cipher/requests/testdata/mutants) is held to
#     the same rule against ./internal/cipher/requests/, whose tests need the Postgres
#     BROKER_TEST_DATABASE_URL names. Where it names none, the mutant is not run: its line says
#     UNKNOWN, and the run exits 3, since a mutant nobody ran is neither caught nor missed and a
#     check that reads this script must not take the run for a pass. --skip-database-mutants
#     makes that a choice: each such mutant's line says SKIPPED, the run lists them at its end,
#     and they alone no longer make it exit 3.
#   - every proxy, policy and requests mutant's named tests are run first on an unpatched copy of
#     the tree and must pass there. One that fails there - a database that refuses connections,
#     a test already broken on the head - would read as a catch once the mutant is applied, so
#     the mutant is not run: its line says UNKNOWN ("its named tests fail without the mutant"),
#     and the run exits 3. A run in which no mutant ran at all exits 3 too.
#
# A mutant nothing catches is a hole in the tests or in the proofs, and this script fails on
# it. The patches are never applied to this repository: every one is applied inside the copy,
# and the run ends with jj's or git's status of the repository to show it is untouched.
#
# Usage: packages/envoy/scripts/airlock-mutants.sh [--skip-database-mutants] [<name>...]
#        with no name, every mutant of the four testdata/mutants directories; a name is a patch's
#        file name, with or without the .patch suffix (for example go-carries-value).
#
# Needs go, GNU patch or git (the patches are plain unified diffs, and `git apply -p1`
# applies them where patch is not installed), and, for the model mutants, lake from the Lean
# toolchain this environment names (ELAN_HOME and PATH are taken as they are); a missing
# lake exits 2. The Go mutants need the built model, and the proxy, policy and requests mutants
# need neither. One PASS:, FAIL:, UNKNOWN: or SKIPPED: line per mutant; the exit is 1 when any
# mutant was not caught, else 3 when any was not run (UNKNOWN), else 0.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$ENVOY_DIR/../.." && pwd)"
AIRLOCK_PKG_DIR="$ENVOY_DIR/internal/cipher/proxyrules"
MUTANT_DIR="$AIRLOCK_PKG_DIR/testdata/mutants"
PROXY_MUTANT_DIR="$ENVOY_DIR/internal/cipher/proxy/testdata/mutants"
POLICY_MUTANT_DIR="$ENVOY_DIR/internal/cipher/policy/testdata/mutants"
REQUESTS_MUTANT_DIR="$ENVOY_DIR/internal/cipher/requests/testdata/mutants"
MODEL="$AIRLOCK_PKG_DIR/lean/.lake/build/bin/airlock-model"

usage() {
	cat <<'EOF'
usage: airlock-mutants.sh [--skip-database-mutants] [<name>...]

Applies each mutant of internal/cipher/proxyrules/testdata/mutants, of
internal/cipher/proxy/testdata/mutants, of internal/cipher/policy/testdata/mutants and of
internal/cipher/requests/testdata/mutants to a copy of packages/envoy under $TMPDIR and
requires that copy to fail: a go-*.patch must fail the comparison fuzz test over the committed
seed corpus, a lean-*.patch must fail `lake build` in a theorem of Airlock/Proofs.lean, because
a proof no longer holds (an error in any other file fails the run), or that
same comparison against the mutated model, and a proxy-*.patch, policy-*.patch or
requests-*.patch must fail every test of its package its own `# must fail: <Test>` lines name,
and is counted by those tests alone, after those tests pass on an unpatched copy; a mutant whose
tests fail there is not run, is reported UNKNOWN, and the run exits 3. A requests-*.patch needs
the Postgres BROKER_TEST_DATABASE_URL names; without one it is not run, reported UNKNOWN, and
the run exits 3.

  --skip-database-mutants   choose not to run the requests-* mutants when
                            BROKER_TEST_DATABASE_URL names no database: each is reported
                            SKIPPED and listed at the end, and they alone do not make the run
                            exit 3
  <name>                    a patch's file name, with or without the .patch suffix (for example
                            go-carries-value or proxy-connect-carried); with no name, every
                            mutant of the four directories

No patch is ever applied to this repository. It prints one PASS:, FAIL:, UNKNOWN: or SKIPPED:
line per mutant and a summary naming each mutant that was not run. It exits 1 when a mutant
was not caught, else 3 when a mutant was not run or when no mutant ran at all, else 0, and 2
when go, lake or both patch and git are missing.
EOF
}

SKIP_DATABASE_MUTANTS=false
names=()
for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	--skip-database-mutants) SKIP_DATABASE_MUTANTS=true ;;
	-*)
		echo "airlock-mutants: unexpected option $arg" >&2
		usage >&2
		exit 2
		;;
	*) names+=("$arg") ;;
	esac
done

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

# mutant_file is the patch a mutant's name names: a proxy-*, policy-* or requests-* mutant's under
# that package's own testdata, every other under the airlock's.
mutant_file() {
	case "$1" in
	proxy-*) echo "$PROXY_MUTANT_DIR/$1.patch" ;;
	policy-*) echo "$POLICY_MUTANT_DIR/$1.patch" ;;
	requests-*) echo "$REQUESTS_MUTANT_DIR/$1.patch" ;;
	*) echo "$MUTANT_DIR/$1.patch" ;;
	esac
}

# The mutants to run: the names given, else every patch of the four testdata/mutants, in name order.
mutants=()
if [[ ${#names[@]} -gt 0 ]]; then
	for name in "${names[@]}"; do
		name="${name%.patch}"
		if [[ ! -f "$(mutant_file "$name")" ]]; then
			echo "airlock-mutants: no mutant named $name in $MUTANT_DIR, $PROXY_MUTANT_DIR, $POLICY_MUTANT_DIR or $REQUESTS_MUTANT_DIR" >&2
			exit 2
		fi
		mutants+=("$name")
	done
else
	for patch_file in "$MUTANT_DIR"/*.patch "$PROXY_MUTANT_DIR"/proxy-*.patch "$POLICY_MUTANT_DIR"/policy-*.patch "$REQUESTS_MUTANT_DIR"/requests-*.patch; do
		mutants+=("$(basename "$patch_file" .patch)")
	done
fi

# database_mutant_unrun answers whether <name> is a mutant whose tests need the Postgres
# BROKER_TEST_DATABASE_URL names, on a run where it names none.
database_mutant_unrun() {
	[[ "$1" == requests-* && -z "${BROKER_TEST_DATABASE_URL:-}" ]]
}

needs_lake=false
needs_model=false
for name in "${mutants[@]}"; do
	case "$name" in
	lean-*) needs_lake=true ;;
	proxy-* | policy-* | requests-*) ;;
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
		tar -C "$ENVOY_DIR" -cf - --exclude=./internal/cipher/proxyrules/lean/.lake . |
			tar -C "$dest/packages" -xf - --one-top-level=envoy
	fi
}

# compare runs the comparison fuzz test over the seed corpus in <copy>, against <model>, and
# answers 0 when the two sides disagreed, 1 when they agreed, 2 when the test did not run.
compare() {
	local copy="$1" model="$2" log="$3"
	if (cd "$copy/packages/envoy" && GOWORK=off AIRLOCK_MODEL="$model" \
		go test -count=1 -run FuzzAirlock ./internal/cipher/proxyrules/) >"$log" 2>&1; then
		return 1
	fi
	# A non-zero exit is the mutant caught only when the comparison itself failed; a copy
	# that does not build or a model that does not answer is a broken run, not a catch.
	if grep -q -- '--- FAIL: FuzzAirlock' "$log"; then
		return 0
	fi
	return 2
}

# failing_seed names the first corpus seed the comparison failed on, for the PASS: line, or the
# fixed cases FuzzAirlock compares before its corpus when the failure came from one of those.
failing_seed() {
	local seed
	seed="$(sed -n 's|^ *--- FAIL: FuzzAirlock/\([^ ]*\).*|\1|p' "$1" | head -n 1)"
	echo "${seed:-none, but a fixed case before the corpus}"
}

# failed_proofs reads a failed `lake build` log of the copy's Lean project <lean dir> and sets
# FAILED_THEOREMS to the theorem of Airlock/Proofs.lean each error is in, once each in the order
# first named, read off the line the error names, and OUTSIDE to each error in another file. A
# mutant is caught by the build only when a proof no longer holds: an error in the model or in
# the program that runs it is a mutant that does not build, which would otherwise read as a
# catch.
failed_proofs() {
	local lean_dir="$1" log="$2" file line theorem
	FAILED_THEOREMS=""
	OUTSIDE=""
	while IFS=: read -r file line; do
		if [[ "$file" != Airlock/Proofs.lean ]]; then
			OUTSIDE="$OUTSIDE $file:$line"
			continue
		fi
		theorem="$(awk -v at="$line" 'NR > at { exit } /^theorem / { name = $2 } END { print name }' "$lean_dir/$file")"
		theorem="${theorem:-(no theorem above $file:$line)}"
		case " $FAILED_THEOREMS " in
		*" $theorem "*) ;;
		*) FAILED_THEOREMS="${FAILED_THEOREMS:+$FAILED_THEOREMS }$theorem" ;;
		esac
	done < <(sed -n 's|^error: \([^:]*\):\([0-9][0-9]*\):.*|\1:\2|p' "$log")
}

# must_fail is the tests a proxy or policy mutant's header names, one `# must fail: <Test>` line
# each, one per output line.
must_fail() {
	sed -n 's|^# must fail: \(Test[A-Za-z0-9_]*\)[[:space:]]*$|\1|p' "$1"
}

# named_tests runs <tests>, the named tests of a proxy or policy mutant, in <package> of <copy>
# and answers 0 when every one of them ran and failed, 1 when one of them passed, 2 when one of
# them did not run - a copy that does not build, or a name no test of the suite has. MISSED is set
# to the ones that passed or did not run.
named_tests() {
	local copy="$1" log="$2" tests="$3" package="$4" test verdict=0
	(cd "$copy/packages/envoy" && GOWORK=off go test -count=1 -v -run "^($(paste -sd '|' <<<"$tests"))\$" "$package") >"$log" 2>&1
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

# baseline_passes runs <tests> in <package> of the unpatched copy, made once for the whole run,
# and answers 0 when every one of them ran and passed there. A named test that fails without the
# mutant - a database that refuses connections, a test broken on the head - would count as a
# catch once the mutant is applied, so a mutant is counted only after its tests pass here. BROKEN
# is set to the ones that failed or did not run.
baseline_passes() {
	local tests="$1" package="$2" log="$3" test passes=0
	if [[ ! -d "$WORK_DIR/.unpatched" ]]; then
		copy_envoy "$WORK_DIR/.unpatched" false
	fi
	(cd "$WORK_DIR/.unpatched/packages/envoy" && GOWORK=off go test -count=1 -v -run "^($(paste -sd '|' <<<"$tests"))\$" "$package") >"$log" 2>&1
	BROKEN=""
	while read -r test; do
		if ! grep -q "^--- PASS: $test " "$log" || grep -q "^--- FAIL: $test " "$log"; then
			BROKEN="$BROKEN $test"
			passes=1
		fi
	done <<<"$tests"
	return "$passes"
}

failures=0
unrun=()
skipped=()
for name in "${mutants[@]}"; do
	# A mutant whose tests cannot run here is set aside before anything is copied or applied.
	if database_mutant_unrun "$name"; then
		if [[ "$SKIP_DATABASE_MUTANTS" == true ]]; then
			echo "SKIPPED: $name — --skip-database-mutants: its tests need the Postgres BROKER_TEST_DATABASE_URL names, and it names none"
			skipped+=("$name")
		else
			echo "UNKNOWN: $name — not run: its tests need the Postgres BROKER_TEST_DATABASE_URL names, and it names none, so they would skip"
			unrun+=("$name")
		fi
		continue
	fi
	started=$SECONDS
	copy="$WORK_DIR/$name"
	log="$WORK_DIR/$name.log"
	if [[ "$name" == proxy-* || "$name" == policy-* || "$name" == requests-* ]]; then
		case "$name" in
		policy-*) package=./internal/cipher/policy/ ;;
		requests-*) package=./internal/cipher/requests/ ;;
		*) package=./internal/cipher/proxy/ ;;
		esac
		tests="$(must_fail "$(mutant_file "$name")")"
		if [ -z "$tests" ]; then
			echo "FAIL: $name — the patch names no test that must fail (a '# must fail: <Test>' line), so nothing it could catch would count"
			failures=$((failures + 1))
			continue
		fi
		if ! baseline_passes "$tests" "$package" "$WORK_DIR/$name.unpatched.log"; then
			echo "UNKNOWN: $name — its named tests fail without the mutant:$BROKEN; $(grep -m1 -E 'connect|Error|error' "$WORK_DIR/$name.unpatched.log" | cut -c1-160)"
			unrun+=("$name")
			continue
		fi
	fi
	case "$name" in
	lean-*) copy_envoy "$copy" true ;;
	*) copy_envoy "$copy" false ;;
	esac
	if ! apply_patch "$copy" "$(mutant_file "$name")" "$log"; then
		echo "FAIL: $name — the patch does not apply: $(head -n 3 "$log" | tr '\n' ' ')"
		failures=$((failures + 1))
		continue
	fi

	if [[ "$name" == proxy-* || "$name" == policy-* || "$name" == requests-* ]]; then
		set +e
		named_tests "$copy" "$log" "$tests" "$package"
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
		lean_dir="$copy/packages/envoy/internal/cipher/proxyrules/lean"
		if ! (cd "$lean_dir" && lake build) >"$log" 2>&1; then
			failed_proofs "$lean_dir" "$log"
			reason="$(grep -m1 -E '^error: ' "$log" | cut -c1-120)"
			if [[ -n "$OUTSIDE" ]]; then
				echo "FAIL: $name — the mutant does not build outside the proofs, at$OUTSIDE: ${reason:-see the build log}"
				failures=$((failures + 1))
			elif [[ -z "$FAILED_THEOREMS" ]]; then
				echo "FAIL: $name — lake build failed and named no line of a proof: $(tail -n 3 "$log" | tr '\n' ' ')"
				failures=$((failures + 1))
			else
				echo "PASS: $name — caught by lake build, a proof no longer holds: $FAILED_THEOREMS (${reason#error: }) ($((SECONDS - started))s)"
			fi
			continue
		fi
		model="$copy/packages/envoy/internal/cipher/proxyrules/lean/.lake/build/bin/airlock-model"
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
ran=$((${#mutants[@]} - ${#unrun[@]} - ${#skipped[@]}))
if [[ $failures -gt 0 ]]; then
	echo "$failures of ${#mutants[@]} mutants were not caught"
else
	echo "$ran mutants, each caught"
fi
if [[ ${#skipped[@]} -gt 0 ]]; then
	echo "skipped by --skip-database-mutants, so neither caught nor missed: ${skipped[*]}"
fi
if [[ ${#unrun[@]} -gt 0 ]]; then
	echo "UNKNOWN: ${#unrun[@]} mutant(s) not run, so neither caught nor missed: ${unrun[*]}; a requests-* mutant needs BROKER_TEST_DATABASE_URL to name a Postgres server that answers (or --skip-database-mutants to leave it out on purpose), and a mutant whose named tests fail without it needs those tests fixed first"
fi
# A run that left every mutant out, by --skip-database-mutants or for any other reason, caught
# nothing at all, which is no pass either.
if [[ $ran -eq 0 ]]; then
	echo "UNKNOWN: no mutant ran, so the run caught nothing"
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

if [[ $failures -gt 0 ]]; then
	exit 1
fi
if [[ ${#unrun[@]} -gt 0 || $ran -eq 0 ]]; then
	exit 3
fi
