#!/usr/bin/env bash
# Tests for check-go-work-sum.sh over fixture workspaces. A file proxy serves their requirements,
# so no case reaches the network. Each download uses GOTOOLCHAIN=local and a cold module cache.
#
# With every hash committed, the check passes. It names a missing content hash, a missing go.mod
# hash of an unselected version, and a hash held only outside the workspace. Each case checks
# that the checkout's sum files stay unchanged. The last case kills the check with SIGKILL while
# a stand-in Go is writing to the copy.
#
# Run from anywhere: .github/scripts/check-go-work-sum.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'chmod -R u+w "$work" 2> /dev/null; rm -rf -- "$work"' EXIT

export GOPROXY="file://$work/proxy" GOTOOLCHAIN=local GOFLAGS=-modcacherw GOWORK='' GOENV=off GONOSUMDB='' GOPRIVATE=''

# The proxy serves two dep versions. The older go line in other makes Go read the go.mod of
# dep v1.0.0 even though the workspace selects v1.1.0.
python3 - "$work/proxy" <<'PY'
import json, pathlib, sys, zipfile

proxy = pathlib.Path(sys.argv[1])
for name, version, directives in (
    ("dep", "v1.0.0", "go 1.16\n"),
    ("dep", "v1.1.0", "go 1.21\n"),
    ("other", "v1.0.0", "go 1.16\n\nrequire example.com/dep v1.0.0\n"),
):
    module = f"example.com/{name}"
    at = proxy / module / "@v"
    at.mkdir(parents=True, exist_ok=True)
    mod = f"module {module}\n\n{directives}"
    with (at / "list").open("a") as versions:
        versions.write(f"{version}\n")
    (at / f"{version}.info").write_text(json.dumps({"Version": version, "Time": "2026-01-01T00:00:00Z"}))
    (at / f"{version}.mod").write_text(mod)
    with zipfile.ZipFile(at / f"{version}.zip", "w") as z:
        z.writestr(f"{module}@{version}/go.mod", mod)
        z.writestr(f"{module}@{version}/{name}.go", f"package {name}\n")
PY

# tree <name>: create a workspace using ./a and echo its root. It keeps a Git index, as a
# checkout does, so that the Verification document's negative control can run these cases over
# the script at b836532eab4f, which read the tracked go.sum files with `git ls-files`.
tree() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/a"
  cp "$script_dir/check-go-work-sum.sh" "$root/.github/scripts/"
  printf 'go 1.21\n\nuse ./a\n' > "$root/go.work"
  cat > "$root/a/go.mod" <<'MOD'
module example.com/a

go 1.21

require (
	example.com/dep v1.1.0
	example.com/other v1.0.0
)
MOD
  printf 'package a\n\nimport (\n\t_ "example.com/dep"\n\t_ "example.com/other"\n)\n' > "$root/a/a.go"
  : > "$root/go.work.sum"
  git -C "$root" init -q && git -C "$root" add -A
  echo "$root"
}

seed=$(tree seed)
(cd "$seed" && GOMODCACHE="$work/seed-cache" GOSUMDB=off go mod download all)
selected=$(grep '^example\.com/dep v1\.1\.0 h1:' "$seed/go.work.sum" || true)
unselected=$(grep '^example\.com/dep v1\.0\.0/go\.mod h1:' "$seed/go.work.sum" || true)
check "the seed has the selected dep content hash" "$(is "${selected:+present}" present)"
check "the seed has the unselected dep go.mod hash" "$(is "${unselected:+present}" present)"
if ((FAIL > 0)); then
  summary "fixture seed"
fi

# stage_fixture <root>: track what the case wrote, as a checkout tracks it, and keep a copy of
# its go.work.sum to compare with after the check.
stage_fixture() {
  git -C "$1" add -A
  cp -- "$1/go.work.sum" "$1.committed"
}

# run_check <root>: stage the tree and run the check on a cold cache, recording output and status.
run_check() {
  stage_fixture "$1"
  status=0
  out=$(cd "$1" && TMPDIR="$work" GOMODCACHE="$1.cache" timeout 120 .github/scripts/check-go-work-sum.sh 2>&1) || status=$?
}

assert_unchanged() {
  check "go.work.sum is still the committed file, byte for byte" \
    "$(cmp -s "$1/go.work.sum" "$1.committed" && echo true || echo false)"
  check "no a/go.sum was created" "$([[ ! -e "$1/a/go.sum" ]] && echo true || echo false)"
}

echo "case 1: every hash the workspace needs is committed"
root=$(tree complete)
cp "$seed/go.work.sum" "$root/go.work.sum"
run_check "$root"
check "exits 0 (got $status)" "$(is "$status" 0)"
check "prints the success line" \
  "$(is "$out" 'no checksum is missing: go mod download all in a copy of the root workspace added nothing to go.work.sum or to the go.sum of a')"
assert_unchanged "$root"

echo "case 2: a selected module's content hash is missing"
root=$(tree missing-content)
grep -Fxv -- "$selected" "$seed/go.work.sum" > "$root/go.work.sum"
run_check "$root"
check "exits 1 (got $status)" "$(is "$status" 1)"
check "counts one missing checksum" \
  "$(contains "$out" '^::error file=go\.work\.sum::no committed file holds 1 of the checksums a download in the root workspace needs')"
check "names the selected content hash verbatim" "$(grep -Fxq -- "  $selected" <<< "$out" && echo true || echo false)"
assert_unchanged "$root"

echo "case 3: an unselected version's go.mod hash is missing"
root=$(tree missing-unselected)
grep -Fxv -- "$unselected" "$seed/go.work.sum" > "$root/go.work.sum"
run_check "$root"
check "exits 1 (got $status)" "$(is "$status" 1)"
check "names the unselected go.mod hash verbatim" "$(grep -Fxq -- "  $unselected" <<< "$out" && echo true || echo false)"
assert_unchanged "$root"

echo "case 4: a hash is held only in a go.sum outside the use list"
root=$(tree outside-use)
grep -Fxv -- "$selected" "$seed/go.work.sum" > "$root/go.work.sum"
mkdir "$root/b"
printf 'module example.com/b\n\ngo 1.21\n\nrequire example.com/dep v1.1.0\n' > "$root/b/go.mod"
printf '%s\n' "$selected" > "$root/b/go.sum"
run_check "$root"
check "exits 1 (got $status)" "$(is "$status" 1)"
check "names the hash held only outside the workspace" "$(grep -Fxq -- "  $selected" <<< "$out" && echo true || echo false)"
assert_unchanged "$root"

echo "case 5: SIGKILL while Go writes a checksum leaves the checkout unchanged"
root=$(tree killed)
cp "$seed/go.work.sum" "$root/go.work.sum"
stage_fixture "$root"
mkdir -p "$work/stand-in/bin" "$work/stand-in-root/bin"
cat > "$work/stand-in/bin/go" <<SH
#!/usr/bin/env bash
[ "\$*" = "env GOROOT" ] && { echo "$work/stand-in-root"; exit 0; }
exit 2
SH
cat > "$work/stand-in-root/bin/go" <<'SH'
#!/usr/bin/env bash
set -e
case "$1 $2" in
  'work edit') echo '{"Go":"1.21","Use":[{"DiskPath":"./a"}]}' ;;
  'mod download')
    echo 'example.com/planted v1.0.0 h1:unverified=' >> ./go.work.sum
    touch "$STAND_IN_WROTE"
    exec sleep 60
    ;;
  *) exit 2 ;;
esac
SH
chmod +x "$work/stand-in/bin/go" "$work/stand-in-root/bin/go"
# Give the check its own process group so SIGKILL stops both it and the stand-in Go.
set -m
(cd "$root" && export PATH="$work/stand-in/bin:$PATH" STAND_IN_WROTE="$work/killed.wrote" TMPDIR="$work" &&
  exec .github/scripts/check-go-work-sum.sh > /dev/null 2>&1) &
pid=$!
set +m
for ((i = 0; i < 100; i++)); do
  [[ -e "$work/killed.wrote" ]] && break
  sleep 0.1
done
check "the stand-in wrote a checksum before SIGKILL" "$([[ -e "$work/killed.wrote" ]] && echo true || echo false)"
kill -KILL -- "-$pid" 2> /dev/null || true
status=0
wait "$pid" 2> /dev/null || status=$?
check "the check ends on SIGKILL" "$(is "$status" 137)"
assert_unchanged "$root"

summary "check-go-work-sum.sh and the checkout's unchanged sum files"
