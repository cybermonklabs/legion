#!/usr/bin/env bash
# Tests for `check-image-build-inputs.sh`: every file a Bun build of an image stage reads must be
# put in place by a COPY of that stage above the build, and the check must say what it read.
#
# Each fixture is a repository of its own: a package whose entry imports a file of another
# directory, a Dockerfile whose stage builds it, and an image_build_entries.py that names the
# step. Its check and path model are this tree's, linked in.
#
# Run from anywhere: .github/scripts/check-image-build-inputs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-image-build-inputs.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/app/src" "$root/data" "$root/docker"
  ln -s "$check_script" "$root/.github/scripts/check-image-build-inputs.sh"
  ln -s "$script_dir/workflow_paths.py" "$root/.github/scripts/workflow_paths.py"
  printf '{"name":"app"}\n' > "$root/packages/app/package.json"
  cat > "$root/packages/app/src/main.ts" <<'TS'
import pages from "../../../data/pages.json";
console.log(pages.length);
TS
  printf '[1, 2]\n' > "$root/data/pages.json"
  cat > "$root/.github/scripts/image_build_entries.py" <<'PY'
STAGES = [
    ("docker/app.Dockerfile", "build", "/repo", "bun build src/main.ts",
     [("bundle", "packages/app", "src/main.ts", ["--target", "bun"])]),
]
PY
  echo "$root"
}

# A stage that copies the package and the file it imports, then builds.
write_dockerfile() {
  local root=$1
  cat > "$root/docker/app.Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14 AS build
WORKDIR /repo
COPY packages/app packages/app
COPY data/pages.json data/pages.json
WORKDIR /repo/packages/app
RUN bun build src/main.ts --outdir dist
DOCKERFILE
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-image-build-inputs.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a stage that copies every file its build reads"
root=$(fixture green)
write_dockerfile "$root"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names the step and what its build read" \
  "$(contains "$out" 'docker/app.Dockerfile:6: stage build, `bun build src/main.ts` in packages/app: 2 files read, 2 copied above the step')"
check "sums what it read" "$(contains "$out" '1 steps, 1 builds, 2 files read')"

echo "case: a file the build imports that no COPY puts in place is refused (the planted missing file)"
root=$(fixture missing)
write_dockerfile "$root"
sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step's line, the file and where it belongs" \
  "$(contains "$out" 'app.Dockerfile:5: .*which reads data/pages.json, and no COPY above line 5 puts it at /repo/data/pages.json')"

echo "case: a COPY below the step counts for nothing"
# The late COPY puts the file where the build reads it, so only its place below the step can make
# it count for nothing.
root=$(fixture copied-too-late)
write_dockerfile "$root"
sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
printf 'COPY data/pages.json /repo/data/pages.json\n' >> "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "still names the file" "$(contains "$out" 'which reads data/pages.json')"

echo "case: a COPY that puts the file in another place counts for nothing"
root=$(fixture elsewhere)
write_dockerfile "$root"
sed -i 's|^COPY data/pages.json data/pages.json$|COPY data/pages.json packages/app/pages.json|' "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the place the build reads it from" "$(contains "$out" 'puts it at /repo/data/pages.json')"

echo "case: a directory source puts its contents under the destination, and a file under a directory with a trailing slash"
root=$(fixture forms)
write_dockerfile "$root"
sed -i 's|^COPY data/pages.json data/pages.json$|COPY data data|' "$root/docker/app.Dockerfile"
run_check "$root"
check "a directory copied whole passes" "$(is "$status" 0)"
root=$(fixture slash)
write_dockerfile "$root"
sed -i 's|^COPY data/pages.json data/pages.json$|COPY data/pages.json data/|' "$root/docker/app.Dockerfile"
run_check "$root"
check "a file copied into a directory passes" "$(is "$status" 0)"

echo "case: a bind mount of the RUN puts its source at its target, in each spelling Docker reads"
# The check and check-image-trigger-paths.sh read a mount by one grammar (workflow_paths.py):
# the type is bind when it names none, the source is source= or src=, the target target=, dst= or
# destination=.
for spelling in "type=bind,source=data,target=/repo/data" "src=data,target=/repo/data" "source=data,dst=/repo/data"; do
  root=$(fixture "mount-${spelling//[=,\/]/-}")
  write_dockerfile "$root"
  sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
  sed -i "s|^RUN bun build|RUN --mount=$spelling bun build|" "$root/docker/app.Dockerfile"
  run_check "$root"
  check "a mount --mount=$spelling passes" "$(is "$status" 0)"
done
root=$(fixture mount-from)
write_dockerfile "$root"
sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
sed -i "s|^RUN bun build|RUN --mount=from=build,src=data,target=/repo/data bun build|" "$root/docker/app.Dockerfile"
run_check "$root"
check "a mount from another stage counts for nothing" "$(is "$status" 1)"

echo "case: an instruction word ends at any white space, and a continued line is one instruction, as BuildKit reads them"
# BuildKit's splitCommand splits an instruction at the first run of [\t\v\f\r ] after its word, so a
# tab after COPY or RUN makes the same instruction as a space; the shared grammar reads both.
root=$(fixture tab-copy)
write_dockerfile "$root"
sed -i 's|^COPY data/pages.json data/pages.json$|COPY\tdata/pages.json data/pages.json|' "$root/docker/app.Dockerfile"
run_check "$root"
check "a COPY with a tab after its word passes" "$(is "$status" 0)"
root=$(fixture tab-mount)
write_dockerfile "$root"
sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
sed -i 's|^RUN bun build|RUN\t--mount=type=bind,source=data,target=/repo/data bun build|' "$root/docker/app.Dockerfile"
run_check "$root"
check "a RUN with a tab before its mount passes" "$(is "$status" 0)"
# BuildKit ends a line at a line feed alone (bufio.ScanLines), so a vertical tab, a form feed or a
# lone carriage return after COPY is white space inside the instruction, not the end of its line.
for space in 'vertical tab:\v' 'form feed:\f' 'carriage return:\r'; do
  name=${space%%:*}
  root=$(fixture "copy-${name// /-}")
  write_dockerfile "$root"
  sed -i "s|^COPY data/pages.json data/pages.json\$|COPY${space#*:}data/pages.json data/pages.json|" "$root/docker/app.Dockerfile"
  run_check "$root"
  check "a COPY with a $name after its word passes" "$(is "$status" 0)"
done
root=$(fixture continued)
write_dockerfile "$root"
sed -i 's|^COPY data/pages.json data/pages.json$|COPY\t\\\n    data/pages.json data/pages.json|' "$root/docker/app.Dockerfile"
sed -i 's|^RUN bun build src/main.ts --outdir dist$|RUN \\\n    bun build src/main.ts --outdir dist|' "$root/docker/app.Dockerfile"
run_check "$root"
check "a COPY and a RUN continued on the next line pass" "$(is "$status" 0)"
check "  and the step is named by the line it starts on" "$(contains "$out" 'docker/app.Dockerfile:7: stage build')"

echo "case: a file the .dockerignore drops is in no COPY's reach"
root=$(fixture ignored)
write_dockerfile "$root"
printf 'data\n' > "$root/.dockerignore"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the dropped file" "$(contains "$out" 'which reads data/pages.json')"
printf 'data\n!data/pages.json\n' > "$root/.dockerignore"
run_check "$root"
check "passes once a later ! line takes it back" "$(is "$status" 0)"
# Docker reads a .dockerignore by the same lines, so a vertical tab inside a line ends no pattern.
root=$(fixture ignored-vertical-tab)
write_dockerfile "$root"
printf 'other\vdata\n' > "$root/.dockerignore"
run_check "$root"
check "a .dockerignore line with a vertical tab in it is one pattern, which drops no file" "$(is "$status" 0)"

echo "case: a Bun build no row names is refused"
root=$(fixture unnamed)
write_dockerfile "$root"
printf 'RUN bun run build\n' >> "$root/docker/app.Dockerfile"
cat > "$root/docker/other.Dockerfile" <<'DOCKERFILE'
FROM oven/bun:1.3.14
RUN bun scripts/generate.ts
RUN bun install --frozen-lockfile && bun --version
DOCKERFILE
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the unnamed build of the named stage" \
  "$(contains "$out" 'app.Dockerfile:7: stage build runs a Bun build that no row')"
check "names a script run with Bun in another Dockerfile" \
  "$(contains "$out" 'other.Dockerfile:2: stage #1 runs a Bun build that no row')"
check "leaves an install and a version check alone" "$(is "$(contains "$out" 'other.Dockerfile:3:')" false)"

echo "case: a step this check cannot find, or finds twice, is refused"
root=$(fixture no-step)
write_dockerfile "$root"
sed -i 's|bun build src/main.ts --outdir dist|bun build src/other.ts --outdir dist|' "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the step matches no RUN" "$(contains "$out" 'matches 0 RUN instructions')"
root=$(fixture two-steps)
write_dockerfile "$root"
printf 'RUN bun build src/main.ts --outdir again\n' >> "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the step matches two RUNs" "$(contains "$out" 'matches 2 RUN instructions (lines 6, 7)')"

echo "case: a build that does not run is a build of unknown inputs, never one of none"
root=$(fixture broken)
write_dockerfile "$root"
rm "$root/packages/app/src/main.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says the build exited and what it reads is unknown" "$(contains "$out" 'exited 1, so what the step reads is unknown')"

echo "case: a build script that reports what it read"
root=$(fixture reported)
write_dockerfile "$root"
cat > "$root/packages/app/build.ts" <<'TS'
import { join } from "node:path";
export async function build(_outdir: string): Promise<string[]> {
  return [join(import.meta.dir, "src/main.ts"), join(import.meta.dir, "../../data/pages.json")];
}
TS
cat > "$root/.github/scripts/image_build_entries.py" <<'PY'
STAGES = [
    ("docker/app.Dockerfile", "build", "/repo", "bun build src/main.ts",
     [("reported", "packages/app", "build.ts", ["build"])]),
]
PY
run_check "$root"
check "exits 0 while the stage copies both files" "$(is "$status" 0)"
check "names the function" "$(contains "$out" 'build of packages/app/build.ts: 2 files read, 2 copied above the step')"
sed -i '/data\/pages.json/d' "$root/docker/app.Dockerfile"
run_check "$root"
check "exits 1 once the reported file is not copied" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'which reads data/pages.json')"

echo "case: a table with no row checks nothing, which is a failure"
root=$(fixture empty)
write_dockerfile "$root"
printf 'STAGES = []\n' > "$root/.github/scripts/image_build_entries.py"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says it reads no build" "$(contains "$out" 'names no step, so this check reads no build')"

summary "check-image-build-inputs.sh"
