#!/usr/bin/env bash
# Tests for cache-superseded.sh's version mode, which ci-caches.yaml's bun-superseded job runs once
# a leg of the bun job has saved or restored the bun entry of its architecture and variant, and
# through it for the rules every mode of that script shares: nothing is deleted unless KEEP is among
# this ref's entries, and a run deletes three entries at most and warns of the rest.
#
# Each case runs the real script with a stand-in `gh` first on PATH. It answers `gh cache list`
# with the case's fixture list, as `--json id,key,ref` prints one, logs every call and logs the id
# of each `gh cache delete`, so a case counts the entries the script deleted. It answers every list
# with the whole fixture, whatever --key and --ref asked for, so what keeps another ref's entry and
# a key of another shape is the script's own selection and not the filter of the list. `sleep` is a
# no-op on the script's PATH, so a retry's waits would cost nothing, and every case runs under
# `timeout`, so a script that hung would fail its case instead of hanging the job.
#
# Run from anywhere: .github/scripts/cache-superseded.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
# The stand-in GitHub: every call goes to calls, a list is answered with list.json, and the id of
# each delete goes to deleted.
set -euo pipefail
echo "\$*" >> "$work/calls"
case "\${1:-} \${2:-}" in
  "cache list") cat "$work/list.json" ;;
  "cache delete") echo "\${3:-}" >> "$work/deleted" ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

# retry_fetch sleeps 10, 20, 40 and 80 seconds between its five attempts. Those waits are
# ci-retry.test.sh's to prove, by measuring them; here no attempt is refused, and a refused one
# would cost nothing, since `sleep` is a no-op on this PATH.
cat > "$work/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
chmod +x "$work/bin/sleep"

# fresh: a case's stand-in GitHub, with no list and no call behind it.
fresh() {
  rm -f "$work/list.json"
  : > "$work/calls"
  : > "$work/deleted"
}
# lists <id> <key> <ref>...: the entries the stand-in's list answers with, in that order.
lists() {
  local json='' comma=''
  while [ "$#" -gt 0 ]; do
    json+="$comma{\"id\":$1,\"key\":\"$2\",\"ref\":\"$3\"}"
    comma=,
    shift 3
  done
  printf '[%s]\n' "$json" > "$work/list.json"
}
# deleted <id>: how many deletes of that id the case made.
deleted() { grep -cx -- "$1" "$work/deleted" || true; }
# deletes: how many deletes the case made.
deletes() { grep -c '' "$work/deleted" || true; }
# calls <text>: how many gh calls the case made whose words hold that text.
calls() { grep -c -- "$1" "$work/calls" || true; }

main=refs/heads/main
pull=refs/pull/5331/merge
# oven-sh/setup-bun's own cache key, which that action wrote before this repository passed it
# no-cache: true: bun- and the base64 SHA-1 of the release asset's URL (its getCacheKey), here
# that of bun-v1.4.3/bun-linux-x64.zip.
setup_bun_key='bun-+NUKu9E/U4apAe78eSCZvQ/GSdc='

# run_version <keep> [<family>]: the version mode for that kept key on main, its stdout and stderr
# together in `out` (the job log holds both) and its exit status in `status`.
run_version() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GH_REPO=o/r KEEP="$1" REF="$main" \
    timeout 60 "$script_dir/cache-superseded.sh" version "${2-bun}" 2>&1)
  status=$?
  set -e
}

echo "case: an x64 leg keeps 1.4.4 and deletes this ref's 1.4.3 of its architecture and variant"
fresh
lists 11 bun-Linux-X64-1.4.4 "$main" \
  12 bun-Linux-X64-1.4.3 "$main" \
  13 bun-Linux-X64-baseline-1.4.3 "$main" \
  14 bun-Linux-ARM64-1.4.3 "$main" \
  15 bun-Linux-X64-1.4.3 "$pull" \
  16 bun-Linux-X64-1.4.3-rc "$main" \
  17 "$setup_bun_key" "$main"
run_version bun-Linux-X64-1.4.4
check "the script ends green" "$(is "$status" 0)"
check "it lists this ref's entries under the kept key's prefix up to the version" \
  "$(is "$(calls "cache list --key bun-Linux-X64- --ref $main ")" 1)"
check "it deletes the superseded entry of the same architecture and variant" "$(is "$(deleted 12)" 1)"
check "it keeps the kept entry" "$(is "$(deleted 11)" 0)"
check "it keeps the other variant's entry" "$(is "$(deleted 13)" 0)"
check "it keeps the other architecture's entry" "$(is "$(deleted 14)" 0)"
check "it keeps another ref's entry" "$(is "$(deleted 15)" 0)"
check "it keeps a key of another shape under the same prefix" "$(is "$(deleted 16)" 0)"
check "it keeps oven-sh/setup-bun's own hashed key" "$(is "$(deleted 17)" 0)"
check "and it deletes nothing else" "$(is "$(deletes)" 1)"

echo "case: a baseline leg keeps its variant's 1.4.4 and deletes its variant's 1.4.3 alone"
fresh
lists 21 bun-Linux-X64-baseline-1.4.4 "$main" \
  22 bun-Linux-X64-baseline-1.4.3 "$main" \
  23 bun-Linux-X64-1.4.3 "$main"
run_version bun-Linux-X64-baseline-1.4.4
check "the script ends green" "$(is "$status" 0)"
check "it deletes the baseline variant's superseded entry" "$(is "$(deleted 22)" 1)"
check "it keeps the other variant's entry" "$(is "$(deleted 23)" 0)"
check "and it deletes nothing else" "$(is "$(deletes)" 1)"

echo "case: an arm64 leg deletes its architecture's 1.4.3 and keeps x64's"
fresh
lists 31 bun-Linux-ARM64-1.4.4 "$main" \
  32 bun-Linux-ARM64-1.4.3 "$main" \
  33 bun-Linux-X64-1.4.3 "$main"
run_version bun-Linux-ARM64-1.4.4
check "the script ends green" "$(is "$status" 0)"
check "it deletes the arm64 entry the kept version supersedes" "$(is "$(deleted 32)" 1)"
check "it keeps the x64 entry" "$(is "$(deleted 33)" 0)"
check "and it deletes nothing else" "$(is "$(deletes)" 1)"

echo "case: a kept key that is not among this ref's entries deletes nothing"
fresh
lists 41 bun-Linux-X64-1.4.3 "$main" \
  42 bun-Linux-X64-1.4.4 "$pull"
run_version bun-Linux-X64-1.4.4
check "the script ends green" "$(is "$status" 0)"
check "it says the kept entry is not among this ref's entries" \
  "$(contains "$out" "^::warning::the new bun entry is not among this ref's entries")"
check "it deletes nothing" "$(is "$(deletes)" 0)"

echo "case: a kept key of another shape fails and deletes nothing"
for keep in bun-Linux-X64-latest bun-Linux-X64-1.4 bun-1.4.4 bun-Linux-X64-1.4.4-rc \
  bun-Linux-X64-Baseline-1.4.4 node-Linux-X64-1.4.4 "$setup_bun_key"; do
  fresh
  lists 51 "$keep" "$main" \
    52 bun-Linux-X64-1.4.3 "$main"
  run_version "$keep"
  check "$keep: the script exits 1" "$(is "$status" 1)"
  check "$keep: it names the shape" "$(contains "$out" '^::error::the kept key does not have the shape')"
  check "$keep: it makes no gh call" "$(is "$(calls '')" 0)"
  check "$keep: it deletes nothing" "$(is "$(deletes)" 0)"
done

echo "case: five superseded entries delete three, in the list's order, and warn of the other two"
fresh
lists 61 bun-Linux-X64-1.4.4 "$main" \
  62 bun-Linux-X64-1.4.3 "$main" \
  63 bun-Linux-X64-1.4.2 "$main" \
  64 bun-Linux-X64-1.4.1 "$main" \
  65 bun-Linux-X64-1.4.0 "$main" \
  66 bun-Linux-X64-1.3.9 "$main"
run_version bun-Linux-X64-1.4.4
check "the script ends green" "$(is "$status" 0)"
check "it deletes three entries" "$(is "$(deletes)" 3)"
check "the first three the list gave" \
  "$(is "$(deleted 62)$(deleted 63)$(deleted 64)" 111)"
check "it warns that the other two stay for the next run" \
  "$(contains "$out" '^::warning::2 more bun entries, .* stay for the next run to delete')"

echo "case: the version mode without its family exits 2 naming the usage"
fresh
lists 71 bun-Linux-X64-1.4.4 "$main"
run_version bun-Linux-X64-1.4.4 ''
check "the script exits 2" "$(is "$status" 2)"
check "it names the usage" "$(contains "$out" '^::error::usage: cache-superseded.sh')"
check "it deletes nothing" "$(is "$(deletes)" 0)"

summary "cache-superseded.sh's version mode"
