#!/usr/bin/env bash
# packages/envoy/scripts/airlock-mutants-apply.test.sh
#
# Every mutant patch of airlock-mutants.sh (the testdata/mutants directories of proxyrules, proxy,
# policy and requests) applies to this tree under both tools that read it: GNU patch at --fuzz=0,
# which airlock-mutants.sh runs, and `git apply --check -p1`, its fallback, each from the
# repository root and each writing nothing. Each tool alone passes a patch the other refuses: GNU
# patch places a hunk with no context by its line numbers and the lines it removes, even at
# --fuzz=0, which git apply refuses anywhere but at the end of a file; and patch at its default
# fuzz drops context lines git apply would hold, as policy-judge-not-in-digest.patch needed fuzz 1
# on main before it was regenerated. A patch whose context no longer matches the code it mutates is
# reported by airlock-mutants.sh as "the patch does not apply" and fails k15's negative control,
# and no CI lane runs airlock-mutants.sh; this test needs no database, no model and no Lean
# toolchain, so a CI lane can. It exits 0 when every patch applies, 1 naming each that does not,
# and 3 when it found no patch at all or GNU patch is not installed, which is no pass.
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR source=lib/mutant-package.sh
. "$script_dir/lib/mutant-package.sh"
if ! command -v patch >/dev/null 2>&1; then
	echo "UNKNOWN: GNU patch is not installed, so no mutant patch was held to patch --fuzz=0"
	exit 3
fi
found=0
failed=0
for package in $(mutant_packages); do
	for patch in "$root/packages/envoy/internal/cipher/$package/testdata/mutants/"*.patch; do
		[ -e "$patch" ] || continue
		found=$((found + 1))
		if ! out="$(patch --dry-run --fuzz=0 -p1 -s --batch --forward -d "$root" -i "$patch" 2>&1)"; then
			printf 'FAIL: %s does not apply under patch --fuzz=0: %s\n' "${patch#"$root"/}" "$(printf '%s' "$out" | head -n 2 | tr '\n' ' ')"
			failed=$((failed + 1))
		elif ! out="$(git -C "$root" apply --check -p1 "$patch" 2>&1)"; then
			printf 'FAIL: %s does not apply under git apply: %s\n' "${patch#"$root"/}" "$(printf '%s' "$out" | head -n 2 | tr '\n' ' ')"
			failed=$((failed + 1))
		fi
	done
done
if [ "$found" -eq 0 ]; then
	echo "UNKNOWN: no mutant patch found under $root/packages/envoy/internal/cipher/*/testdata/mutants"
	exit 3
fi
if [ "$failed" -gt 0 ]; then
	echo "airlock-mutants-apply: $failed of $found patches do not apply"
	exit 1
fi
echo "airlock-mutants-apply: all $found patches apply under patch --fuzz=0 and git apply"
