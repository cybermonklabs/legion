#!/usr/bin/env bash
# Tests for check-go-work-sum.sh, over a fixture workspace whose one requirement a file proxy
# serves, so no case reaches the network: every go command runs with GOPROXY at that proxy,
# GOTOOLCHAIN=local and an empty module cache of the case's own.
#
# What the cases hold: a workspace whose hashes are all committed passes; one with a hash removed
# fails naming the module; and either way the module files are left byte for byte as committed.
# In a workspace `go mod download` writes the hashes it computes into go.work.sum, and the check
# downloads with GOSUMDB=off, so a hash it left there would be one no checksum database verified,
# which a developer following its refusal would commit. The last case stops the check while a
# stand-in go is writing go.work.sum, and the file is back as committed.
#
# Run from anywhere: .github/scripts/check-go-work-sum.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
# The module cache is read-only as go leaves it, and GOFLAGS=-modcacherw below makes it writable.
trap 'chmod -R u+w "$work" 2> /dev/null; rm -rf "$work"' EXIT

export GOPROXY="file://$work/proxy" GOTOOLCHAIN=local GOFLAGS=-modcacherw GOWORK= GOENV=off GONOSUMDB= GOPRIVATE=

# The proxy: example.com/dep v1.0.0, one package, laid out as a module proxy serves it.
mkdir -p "$work/proxy/example.com/dep/@v"
python3 - "$work/proxy/example.com/dep/@v" <<'PY'
import pathlib, sys, zipfile
at = pathlib.Path(sys.argv[1])
mod = "module example.com/dep\n\ngo 1.21\n"
(at / "list").write_text("v1.0.0\n")
(at / "v1.0.0.info").write_text('{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}\n')
(at / "v1.0.0.mod").write_text(mod)
with zipfile.ZipFile(at / "v1.0.0.zip", "w") as z:
    z.writestr("example.com/dep@v1.0.0/go.mod", mod)
    z.writestr("example.com/dep@v1.0.0/dep.go", "package dep\n")
PY

# tree <name>: a workspace under $work/<name> using ./a, which requires example.com/dep, with
# the script under test at its .github/scripts and every file in git's index, as a checkout has
# them. Echoes its root. Its go.work.sum is empty; the cases write the one they test.
tree() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/a"
  cp "$script_dir/check-go-work-sum.sh" "$root/.github/scripts/"
  printf 'go 1.21\n\nuse ./a\n' > "$root/go.work"
  printf 'module example.com/a\n\ngo 1.21\n\nrequire example.com/dep v1.0.0\n' > "$root/a/go.mod"
  printf 'package a\n\nimport _ "example.com/dep"\n' > "$root/a/a.go"
  : > "$root/go.work.sum"
  git -C "$root" init -q
  git -C "$root" add -A
  echo "$root"
}

# The two hashes of example.com/dep v1.0.0, as a download computes them into go.work.sum.
seed=$(tree seed)
(cd "$seed" && GOMODCACHE="$work/seed-cache" GOSUMDB=off go mod download all)
hashes=$(cat "$seed/go.work.sum" "$seed/a/go.sum" 2> /dev/null | grep '^example.com/dep v1.0.0' | sort || true)
check "the fixture's download computes both hashes of example.com/dep" "$(is "$(grep -c . <<< "$hashes")" 2)"

# run_check <root>: the check in the tree, on a cold module cache of its own, with its output in
# `out` and its status in `status`.
run_check() {
  set +e
  out=$(cd "$1" && GOMODCACHE="$1.cache" timeout 120 .github/scripts/check-go-work-sum.sh 2>&1)
  status=$?
  set -e
}

echo "case: every hash the workspace needs is committed"
root=$(tree complete)
printf '%s\n' "$hashes" > "$root/go.work.sum"
cp "$root/go.work.sum" "$work/complete.committed"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the one module it checked" "$(contains "$out" "every checksum the workspace's 1 modules need is committed")"
check "and leaves go.work.sum as committed" "$(cmp -s "$root/go.work.sum" "$work/complete.committed" && echo true || echo false)"

echo "case: a hash is missing on a cold module cache"
root=$(tree missing)
grep '/go.mod ' <<< "$hashes" > "$root/go.work.sum"
cp "$root/go.work.sum" "$work/missing.committed"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "naming the module whose hash no file holds" \
  "$(contains "$out" "^::error file=go.work.sum::1 of the 1 modules this workspace needs have a checksum in no committed file")"
check "and the module itself" "$(contains "$out" '^  example.com/dep v1.0.0$')"
check "go.work.sum is still the committed file, byte for byte" \
  "$(cmp -s "$root/go.work.sum" "$work/missing.committed" && echo true || echo false)"
check "and no go.sum the tree did not have is left" "$(is "$([ -e "$root/a/go.sum" ] && echo left || echo none)" none)"

# A stand-in go: `go env GOROOT` names a GOROOT whose go writes a line into go.work.sum, as a
# download does, then waits to be stopped.
echo "case: a run stopped mid-download leaves go.work.sum as committed"
root=$(tree stopped)
printf '%s\n' "$hashes" > "$root/go.work.sum"
cp "$root/go.work.sum" "$work/stopped.committed"
mkdir -p "$work/stand-in/bin" "$work/stand-in-root/bin"
cat > "$work/stand-in/bin/go" <<SH
#!/usr/bin/env bash
[ "\$*" = "env GOROOT" ] && { echo "$work/stand-in-root"; exit 0; }
exit 2
SH
cat > "$work/stand-in-root/bin/go" <<'SH'
#!/usr/bin/env bash
echo "example.com/planted v1.0.0 h1:unverified=" >> go.work.sum
touch "$STAND_IN_WROTE"
sleep 60
SH
chmod +x "$work/stand-in/bin/go" "$work/stand-in-root/bin/go"
# Its own process group (job control on), so the TERM reaches the check and the go it waits on
# together, as a Ctrl-C or a cancelled job's signal reaches a terminal's foreground group: bash
# runs a trap only once the command it waits on has ended.
set -m
(cd "$root" && PATH="$work/stand-in/bin:$PATH" STAND_IN_WROTE="$work/stopped.wrote" \
  exec .github/scripts/check-go-work-sum.sh > "$work/stopped.out" 2>&1) &
pid=$!
set +m
for _ in $(seq 100); do [ -e "$work/stopped.wrote" ] && break; sleep 0.1; done
check "the stand-in wrote into go.work.sum" "$(is "$([ -e "$work/stopped.wrote" ] && echo wrote || echo none)" wrote)"
kill -TERM -- "-$pid"
set +e
wait "$pid"
status=$?
set -e
check "the check ends on the TERM" "$(is "$status" 143)"
check "and go.work.sum is the committed file again, byte for byte" \
  "$(cmp -s "$root/go.work.sum" "$work/stopped.committed" && echo true || echo false)"

summary "check-go-work-sum.sh's checks and the module files it leaves"
