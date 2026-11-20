#!/usr/bin/env bash
# Tests for `check-ts-filters.sh`: every file a gate's tsc program compiles must be selected by
# that gate's filter, the files outside the repository and under node_modules dropped and a
# relative path resolved against the repository root, with a program that cannot be listed, a
# pair naming a filter the changes job does not define, a pair naming a path the repository does
# not hold, a `!` pattern of a covered filter read under the step's quantifier - refused when
# it excludes a file of the program, passed over when it excludes files outside it - a refusal
# of the engine's, written to the log as the workflow command the engine writes for it, a
# filters file that is absent, which ends the run naming the file, and a list read whole: in
# full where a pipe would cut it, and refused by its line where it is empty, cut or names no file
# (an empty line, or a path outside the repository that is not there, among them), or keeps no
# file of the program's own directory.
# The same holds for every module a bundle loads, with a bundle refused by its entry when it does
# not build (its report read whole where a pipe would cut it), leaves no metafile, or leaves one
# naming no module of the repository or one that is no file, each refusal showing the scratch
# directory as <scratch>; and a listed block holds exactly the files its sources give, the union
# where it names two of them, a tsc program and a bundle, as the browser suite's block of the real
# ci_ts_programs.py does.
#
# Each case builds a miniature repository under a temporary directory - a ci.yaml whose changes
# job hands its paths-filter step .github/ci-filters.yaml, that file defining two filters through
# an anchor, a package and a library it compiles, and a
# ci_ts_programs.py pairing them - and symlinks the real script, the engine it borrows the
# changes job's filters from (ci_filter_coverage.py), the path model both import
# (workflow_paths.py) and the Go build-tag module the engine imports (go_build_tags.py) into it,
# so the code under test is the code CI runs. The programs are
# listed by a stand-in bunx on the fixture's PATH, which prints the list the fixture wrote for
# the project, and the bundles built by a stand-in bun beside it, which writes the metafile of
# the list the fixture wrote beside the entry: a case's input is then the file list itself, and
# the harness needs no Bun, no node_modules and no tsc. It needs a python3 with PyYAML.
#
# Run from anywhere: .github/scripts/check-ts-filters.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes. The app job is gated on the filter app, which names its own
# package and, through the shared anchor, the library it compiles; pkg/app/listed is what the
# stand-in bunx answers for that project, and it holds a file of each kind the script must drop
# or keep. Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/bin" "$root/pkg/app/src" "$root/pkg/lib" \
    "$root/other"
  for linked in check-ts-filters.sh ci_filter_coverage.py workflow_paths.py go_build_tags.py; do
    ln -s "$script_dir/$linked" "$root/.github/scripts/$linked"
  done
  echo '{}' > "$root/pkg/app/tsconfig.json"
  printf 'export const app = 1;\n' > "$root/pkg/app/src/main.ts"
  printf 'export const lib = 1;\n' > "$root/pkg/lib/lib.ts"
  printf 'export const other = 1;\n' > "$root/other/thing.ts"
  printf 'export const generated = 1;\n' > "$root/pkg/app/src/generated.ts"
  mkdir -p "$root/node_modules/typescript/lib" "$work/outside/typescript/lib"
  : > "$root/node_modules/typescript/lib/lib.es5.d.ts"
  : > "$work/outside/typescript/lib/lib.dom.d.ts"
  # A file of the package, a file of the library, a path tsc printed relative to the repository
  # root, a file under node_modules and a file outside the repository: the last two are no part
  # of what a filter must select, and the three before them are. Each is a file on disk, as each
  # file tsc lists is: a line that names none is a refusal of its own.
  cat > "$root/pkg/app/listed" <<LIST
pkg/app/src/main.ts
pkg/lib/lib.ts
./pkg/app/src/generated.ts
node_modules/typescript/lib/lib.es5.d.ts
$work/outside/typescript/lib/lib.dom.d.ts
LIST
  cat > "$root/bin/bunx" <<'SH'
#!/usr/bin/env bash
# A stand-in bunx for the fixtures: `bunx tsc -p <project> --noEmit --listFilesOnly` prints the
# lines of <project>/listed, each made absolute against the fixture root unless it already starts
# with / or ./, as tsc prints absolute paths and the script must read either; an empty line it
# prints empty. With a <project>/fails file it prints that file and exits 2, as tsc does on a
# project it cannot read.
# It writes the list in two writes, split at the byte that <project>/cut or <project>/pipe-cut
# names, and exits 0. With `cut` the second write is lost. With `pipe-cut` it is lost when stdout
# is a pipe, as Bun loses what a full pipe has not taken when tsc exits (check-ts-filters.sh, THE
# WHOLE OUTPUT), and it lands when stdout is a regular file, where Bun's writes wait until done.
set -euo pipefail
if [ "$1" != tsc ] || [ "$2" != -p ]; then
  echo "stand-in bunx: unexpected argv: $*" >&2
  exit 64
fi
project=$3
if [ -f "$project/fails" ]; then
  cat "$project/fails" >&2
  exit 2
fi
whole=$(mktemp)
trap 'rm -f "$whole"' EXIT
while IFS= read -r line; do
  case $line in
    '') printf '\n' ;;
    /* | ./*) printf '%s\n' "$line" ;;
    *) printf '%s\n' "$PWD/$line" ;;
  esac
done < "$project/listed" > "$whole"
split=$(wc -c < "$whole") lost=false
if [ -f "$project/cut" ]; then
  split=$(<"$project/cut") lost=true
elif [ -f "$project/pipe-cut" ]; then
  split=$(<"$project/pipe-cut")
  if [ -p /dev/stdout ]; then lost=true; fi
fi
head -c "$split" "$whole"
"$lost" || tail -c "+$((split + 1))" "$whole"
SH
  chmod +x "$root/bin/bunx"
  cat > "$root/bin/bun" <<'SH'
#!/usr/bin/env bash
# A stand-in bun for the fixtures: `bun build <entry> --target bun --outdir <dir>
# --metafile=<dir>/meta.json` writes <dir>/meta.json with one `inputs` key per line of the file
# `bundled` beside <entry>, each a path relative to the directory it runs in, as Bun writes them,
# so a line may reach into node_modules or climb out of the fixture. It records <dir> in the file
# `outdir` beside <entry>, so a case can tell whether a refusal shows that directory. With a
# `fails` file beside <entry> it prints that file, each @outdir@ in it written as <dir>, and exits
# 1, as Bun does on an entry it cannot build: in two writes, the first line and then the rest,
# where a `pipe-cut` file beside <entry> makes it lose the second when stderr is a pipe, as Bun
# loses what a full pipe has not taken when it exits. With a `nometa` file beside it, it exits 0
# and writes no metafile.
set -euo pipefail
if [ "$#" -ne 7 ] || [ "$1" != build ] || [ "$3" != --target ] || [ "$4" != bun ] || [ "$5" != --outdir ] \
  || [ "$7" != "--metafile=$6/meta.json" ]; then
  echo "stand-in bun: unexpected argv: $*" >&2
  exit 64
fi
beside=$(dirname "$2")
printf '%s\n' "$6" > "$beside/outdir"
if [ -f "$beside/fails" ]; then
  lines=()
  while IFS= read -r line; do
    lines+=("${line//@outdir@/"$6"}")
  done < "$beside/fails"
  printf '%s\n' "${lines[0]}" >&2
  if [ ! -f "$beside/pipe-cut" ] || [ ! -p /dev/stderr ]; then
    if [ "${#lines[@]}" -gt 1 ]; then printf '%s\n' "${lines[@]:1}" >&2; fi
  fi
  exit 1
fi
if [ -f "$beside/nometa" ]; then
  exit 0
fi
mkdir -p "$6"
separator=
{
  printf '{"inputs": {'
  while IFS= read -r line; do
    printf '%s"%s": {"bytes": 0, "imports": []}' "$separator" "$line"
    separator=', '
  done < "$beside/bundled"
  printf '}, "outputs": {}}\n'
} > "$6/meta.json"
SH
  chmod +x "$root/bin/bun"
  cat > "$root/.github/scripts/ci_ts_programs.py" <<'PY'
PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]
BUNDLES = []
LISTED = []
PY
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: [pull_request]
jobs:
  changes:
    runs-on: ubuntu-24.04
    steps:
      - uses: dorny/paths-filter@v4
        id: filter
        with:
          predicate-quantifier: some-with-excludes
          filters: .github/ci-filters.yaml
YAML
  cat > "$root/.github/ci-filters.yaml" <<'YAML'
library: &library
  - 'pkg/lib/**'
app:
  - '.github/workflows/ci.yaml'
  - *library
  - 'pkg/app/**'
other:
  - 'other/**'
YAML
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(PATH="$root/bin:$PATH" "$root/.github/scripts/check-ts-filters.sh" 2>&1)
  status=$?
  set -e
}

# replace <file> <old> <new>: one literal replacement, failing the case loudly if <old> is absent.
replace() {
  local file=$1 old=$2 new=$3 text
  text=$(<"$file")
  if [[ $text != *"$old"* ]]; then
    echo "  FAIL: $file has no '$old' to replace" >&2
    return 1
  fi
  printf '%s\n' "${text/"$old"/"$new"}" > "$file"
}

echo "case: the filter selects every file of the program, node_modules and the files outside the repository dropped"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the three files it kept" "$(contains "$out" '^pkg/app: 3 files, 0 uncovered (filter app)$')"
check "says what it checked" "$(contains "$out" 'every changes filter selects every file its job.s typecheck compiles (1 programs, 1 filters)')"

echo "case: a file of another package the filter does not select"
root=$(fixture uncovered-file)
replace "$root/.github/ci-filters.yaml" "  - 'pkg/lib/**'" "  - 'other/**'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the file and the program it came from" \
  "$(contains "$out" '::error file=pkg/lib/lib.ts::filter app does not select pkg/lib/lib.ts, which the tsc program of pkg/app compiles (app: Typecheck the app)')"
check "says the filter must select it or no job starts" \
  "$(contains "$out" 'the filter must select it, or the change that breaks that program starts no job')"
check "counts it" "$(contains "$out" '^pkg/app: 3 files, 1 uncovered (filter app)$')"

echo "case: a program tsc cannot list"
root=$(fixture unlistable-program)
printf 'error TS5083: cannot read file tsconfig.json\n' > "$root/pkg/app/fails"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and what it printed" \
  "$(contains "$out" '::error file=pkg/app::`bunx tsc -p pkg/app --noEmit --listFilesOnly` failed (exit 2), so the files its program holds are unknown: error / TS5083')"

# Bun loses what a full pipe has not taken when the program exits (check-ts-filters.sh, THE
# WHOLE OUTPUT), so a reader that comes late, as one on a loaded machine does, gets the list cut
# at any byte after an exit of 0. The stand-in takes that cut wherever its stdout is a pipe: the
# check passes these two cases only if it reads tsc's output from a regular file. A cut in the
# middle of a line made the run that found this name 'pa' as a file no filter selects; a cut at
# the end of a line leaves a list with no sign of the cut, which a check that reads it passes.
echo "case: a tsc list that a pipe cuts in the middle of a line"
root=$(fixture pipe-cut-mid-line)
printf '%s\n' "$(( $(printf '%s\n%s' "$root/pkg/app/src/main.ts" "$root/pk" | wc -c) ))" > "$root/pkg/app/pipe-cut"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the three files of the whole list" "$(contains "$out" '^pkg/app: 3 files, 0 uncovered (filter app)$')"
check "takes no cut line for a file" "$(is "$(contains "$out" 'does not select pk')" false)"

echo "case: a tsc list that a pipe cuts at the end of a line"
root=$(fixture pipe-cut-line-end)
printf '%s\n' "$(( $(printf '%s\n%s\n' "$root/pkg/app/src/main.ts" "$root/pkg/lib/lib.ts" | wc -c) ))" \
  > "$root/pkg/app/pipe-cut"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the three files of the whole list, not the two before the cut" \
  "$(contains "$out" '^pkg/app: 3 files, 0 uncovered (filter app)$')"

# Over the whole read, a list is held whole: a cut a pipe did not make, a line that names no
# file and an empty list are each a refusal by name, never a file no filter selects or a program
# of fewer files.
echo "case: a tsc list whose last line has no newline"
root=$(fixture cut-mid-line)
printf '%s\n' "$(( $(printf '%s\n%s' "$root/pkg/app/src/main.ts" "$root/pk" | wc -c) ))" > "$root/pkg/app/cut"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and the cut line" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and its last line, '$root/pk', has no newline at its end, so its output is cut and the files its program holds are unknown")"
check "takes the cut line for no file" "$(is "$(contains "$out" 'does not select')" false)"
check "counts that one problem" "$(contains "$out" '^::error::1 problem(s) across 1 tsc program(s), 0 bundle(s) and 0 listed block(s)$')"

echo "case: a tsc list holding lines that name no file"
root=$(fixture nameless-lines)
printf 'pkg/app/src/main.ts\nbun add v1.4.4\npkg/app/src/gone.ts\npkg/lib/lib.ts\n' > "$root/pkg/app/listed"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names each line that names no file" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and printed 2 line(s) that name no file this check can find: '$root/bun add v1.4.4', '$root/pkg/app/src/gone.ts', so the list is cut or holds a name that is no path, and the files its program holds are unknown")"
check "takes neither for a file no filter selects" "$(is "$(contains "$out" 'does not select')" false)"
check "holds the filter to no list" "$(is "$(contains "$out" '^pkg/app: ')" false)"

# A line outside the repository is held to being a file too (tsc lists only files it read), and an
# empty line is a line that names no file, never one passed over: either may be what a cut left.
echo "case: a tsc list naming a file outside the repository that is not there"
root=$(fixture outside-missing)
printf 'pkg/app/src/main.ts\n%s\npkg/lib/lib.ts\n' "$work/outside/typescript/lib/lib.gone.d.ts" > "$root/pkg/app/listed"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the line outside the repository that names no file" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and printed 1 line(s) that name no file this check can find: '$work/outside/typescript/lib/lib.gone.d.ts', so the list is cut or holds a name that is no path")"
check "holds the filter to no list" "$(is "$(contains "$out" '^pkg/app: ')" false)"

echo "case: a tsc list holding an empty line"
root=$(fixture empty-line)
printf 'pkg/app/src/main.ts\n\npkg/lib/lib.ts\n' > "$root/pkg/app/listed"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the empty line as a line that names no file" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and printed 1 line(s) that name no file this check can find: '', so the list is cut or holds a name that is no path")"
check "holds the filter to no list" "$(is "$(contains "$out" '^pkg/app: ')" false)"

echo "case: a tsc that exits 0 and prints nothing"
root=$(fixture empty-list)
: > "$root/pkg/app/listed"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command and says a program holds at least the library files" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and printed nothing, so the files its program holds are unknown: a program holds at least TypeScript's own library files")"
check "holds the filter to no list" "$(is "$(contains "$out" '^pkg/app: ')" false)"

# A program holds at least its own sources, so a list whose kept files are none of them - a list
# cut at a line end after TypeScript's own library files, which the newline guard cannot see - is
# a refusal by name, never a program of no files that every filter covers.
echo "case: a tsc list naming no file of the program's own directory"
root=$(fixture no-own-file)
printf 'node_modules/typescript/lib/lib.es5.d.ts\n%s\n' "$work/outside/typescript/lib/lib.dom.d.ts" \
  > "$root/pkg/app/listed"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the command, the directory and what a program holds at least" \
  "$(contains "$out" "::error file=pkg/app::\`bunx tsc -p pkg/app --noEmit --listFilesOnly\` exited 0 and its list keeps 0 file(s) of this repository, none of them under pkg/app, the program's own directory, so the list is cut or is another program's, and the files its program holds are unknown: a program holds at least its own sources")"
check "holds the filter to no list" "$(is "$(contains "$out" '^pkg/app: ')" false)"
printf 'pkg/lib/lib.ts\n' > "$root/pkg/app/listed"
run_check "$root"
check "one keeping only another directory's files fails the same way" \
  "$(contains "$out" "exited 0 and its list keeps 1 file(s) of this repository, none of them under pkg/app, the program's own directory")"
# A directory whose name starts with the program's directory's name is another directory:
# pkg/application is no part of pkg/app, so its files are not the program's own.
mkdir -p "$root/pkg/application"
printf 'export const application = 1;\n' > "$root/pkg/application/main.ts"
printf 'pkg/application/main.ts\n' > "$root/pkg/app/listed"
run_check "$root"
check "one keeping only a sibling whose name starts with the program's directory fails the same way" \
  "$(contains "$out" "exited 0 and its list keeps 1 file(s) of this repository, none of them under pkg/app, the program's own directory")"

echo "case: a pair naming a filter the changes job does not define"
root=$(fixture unknown-filter)
replace "$root/.github/scripts/ci_ts_programs.py" '("app", "pkg/app"' '("application", "pkg/app"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter and the program it holds to nothing" \
  "$(contains "$out" '::error file=.github/scripts/ci_ts_programs.py::application is no filter of the changes job, so the program pkg/app')"

echo "case: a pair naming a path the repository does not hold"
root=$(fixture missing-project)
replace "$root/.github/scripts/ci_ts_programs.py" '"pkg/app"' '"pkg/gone"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the path and the filter it leaves holding no program" \
  "$(contains "$out" '::error file=.github/scripts/ci_ts_programs.py::pkg/gone (app: Typecheck the app) is no path of this repository, so filter app is held to no program')"

echo "case: a ! pattern excluding a file the program compiles"
root=$(fixture negated-pattern)
replace "$root/.github/ci-filters.yaml" "  - 'pkg/app/**'" $'  - \'pkg/app/**\'\n  - \'!pkg/app/src/generated.ts\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the file and the program it came from" \
  "$(contains "$out" '::error file=pkg/app/src/generated.ts::filter app does not select pkg/app/src/generated.ts, which the tsc program of pkg/app compiles (app: Typecheck the app)')"
check "counts it" "$(contains "$out" '^pkg/app: 3 files, 1 uncovered (filter app)$')"

# The patterns rule 17 of check-ci-filters.sh writes into most gate filters are of this shape,
# and they are why this check reads a filter under the quantifier the step declares instead of
# refusing one that has a `!` pattern at all.
echo "case: a ! pattern excluding files the program does not compile"
root=$(fixture negated-pattern-outside)
replace "$root/.github/ci-filters.yaml" "  - 'pkg/app/**'" $'  - \'pkg/app/**\'\n  - \'!pkg/app/**/*_test.go\''
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "holds every file of the program to the filter still" \
  "$(contains "$out" '^pkg/app: 3 files, 0 uncovered (filter app)$')"

# This check reads the changes job's filters through the engine of check-ci-filters.sh, which
# appends its own refusals to this check's problems as Annotation values. Each must reach the log
# as the workflow command the engine writes for it, never as the value's Python repr.
echo "case: a refusal of the engine's, a paths-filter step under a quantifier it does not model"
root=$(fixture engine-refusal)
cat >> "$root/.github/workflows/ci.yaml" <<'YAML'
      - uses: dorny/paths-filter@v4
        id: filter2
        with:
          predicate-quantifier: every
          filters: .github/ci-filters.yaml
YAML
run_check "$root"
check "fails" "$(is "$status" 1)"
check "writes the engine's refusal as a workflow command naming the step and the quantifier" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml::the changes job.s paths-filter step filter2 has predicate-quantifier every, which this check does not model$')"
check "writes no Python repr of an annotation" "$(is "$(contains "$out" 'Annotation(')" false)"
check "counts it in the summary" "$(contains "$out" '^::error::1 problem(s) across 1 tsc program(s), 0 bundle(s) and 0 listed block(s)$')"

# The engine's reader of .github/ci-filters.yaml refuses a file that is not there rather than
# reading it as no filter, and this check writes that refusal as the workflow command the engine
# writes for it.
echo "case: the filters file is absent"
root=$(fixture filters-absent)
rm "$root/.github/ci-filters.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file and says it is absent" \
  "$(contains "$out" '^::error file=.github/ci-filters.yaml::cannot read repository input: .github/ci-filters.yaml is absent')"
check "prints no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# listed <root>: the fixture turned into one with a listed block. The filter consumer gates a job
# with no program of its own that loads what app's program compiles of pkg/lib, so LISTED takes
# its files under pkg/lib from app's program, and pkg/lib/unused.ts, which that program does not
# compile, must stay out of the block. The block starts empty, for the write mode to fill.
listed() {
  local root=$1
  printf 'export const unused = 1;\n' > "$root/pkg/lib/unused.ts"
  cat > "$root/.github/scripts/ci_ts_programs.py" <<'PY'
PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]
BUNDLES = []
LISTED = [("consumer", "pkg/lib", ("app",), "consumer loads what app compiles")]
PY
  replace "$root/.github/ci-filters.yaml" $'\nother:' \
    $'\nconsumer:\n  - \'pkg/app/**\'\n  # BEGIN listed\n  # END listed\nother:'
}

run_write() {
  local root=$1
  set +e
  out=$(PATH="$root/bin:$PATH" "$root/.github/scripts/check-ts-filters.sh" write 2>&1)
  status=$?
  set -e
}

echo "case: the write mode writes a listed block of the files the source filter's programs compile"
root=$(fixture listed-write)
listed "$root"
run_write "$root"
written=$(<"$root/.github/ci-filters.yaml")
check "exits 0" "$(is "$status" 0)"
check "names the command that writes the block" \
  "$(contains "$written" '^  # BEGIN listed by `.github/scripts/check-ts-filters.sh write`: the files of pkg/lib filter app.s$')"
check "lists the file the program compiles" "$(contains "$written" "^  - 'pkg/lib/lib.ts'$")"
check "leaves out the file it does not" "$(is "$(contains "$written" 'unused')" false)"
check "closes the block" "$(contains "$written" '^  # END listed$')"
check "says what it wrote" "$(contains "$out" '^filter consumer: wrote 1 listed file(s) under pkg/lib, from filter app.s tsc programs$')"
run_check "$root"
check "the check passes over what it wrote" "$(is "$status" 0)"
check "says it held the block" "$(contains "$out" 'every listed block holds exactly the files its programs compile and its bundles load (1 blocks)')"

echo "case: a listed block holding a file the programs do not compile"
root=$(fixture listed-extra)
listed "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" "  - 'pkg/lib/lib.ts'" $'  - \'pkg/lib/lib.ts\'\n  - \'pkg/lib/unused.ts\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the extra file and the command that writes the block" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer's listed block has 'pkg/lib/unused.ts', which no tsc program of filter app compiles, .*run \`.github/scripts/check-ts-filters.sh write\`")"

echo "case: a listed block lacking a file the programs compile"
root=$(fixture listed-missing)
listed "$root"
printf 'pkg/app/src/main.ts\npkg/lib/lib.ts\npkg/lib/more.ts\n' > "$root/pkg/app/listed"
printf 'export const more = 1;\n' > "$root/pkg/lib/more.ts"
run_write "$root"
replace "$root/.github/ci-filters.yaml" $'  - \'pkg/lib/more.ts\'\n' ''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the missing file and the command that writes the block" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer's listed block lacks 'pkg/lib/more.ts', which the tsc programs of filter app compile (consumer loads what app compiles): .*run \`.github/scripts/check-ts-filters.sh write\`")"

echo "case: a pattern outside the listed block matching a file of its tree"
root=$(fixture listed-loose)
listed "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" $'\nconsumer:\n' $'\nconsumer:\n  - \'pkg/lib/**\'\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the pattern and a file of the tree it matches" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer has 'pkg/lib/\*\*' outside its listed block, and it matches pkg/lib/lib.ts under pkg/lib")"

echo "case: a listed block in a filter LISTED does not name"
root=$(fixture listed-stray)
replace "$root/.github/ci-filters.yaml" "  - 'other/**'" $'  - \'other/**\'\n  # BEGIN listed\n  - \'other/thing.ts\'\n  # END listed'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter and says nothing computes its block" \
  "$(contains "$out" '::error file=.github/ci-filters.yaml::other has a listed block, and LISTED of .github/scripts/ci_ts_programs.py names no such filter')"

# bundled <root>: the fixture with a CLI, pkg/cli/main.ts, which the job gated on the filter cli
# runs with Bun. pkg/cli/bundled is what the stand-in bun answers for that entry: the entry, a
# module of the library app's program does not compile (pkg/lib/runtime.ts), a module under
# node_modules and a path outside the repository, the last two no part of what a filter must
# select. BUNDLES pairs the entry with cli, and LISTED takes cli's files under pkg/lib from its
# own bundle, so pkg/lib/lib.ts, which app's program alone compiles, and pkg/lib/unused.ts, which
# nothing loads, stay out of its block. The block starts empty, for the write mode to fill.
bundled() {
  local root=$1
  mkdir -p "$root/pkg/cli" "$root/node_modules/zod" "$work/outside"
  printf 'export const cli = 1;\n' > "$root/pkg/cli/main.ts"
  printf 'export const runtime = 1;\n' > "$root/pkg/lib/runtime.ts"
  printf 'export const unused = 1;\n' > "$root/pkg/lib/unused.ts"
  : > "$root/node_modules/zod/index.js"
  : > "$work/outside/elsewhere.ts"
  cat > "$root/pkg/cli/bundled" <<'LIST'
pkg/cli/main.ts
pkg/lib/runtime.ts
node_modules/zod/index.js
../outside/elsewhere.ts
LIST
  cat > "$root/.github/scripts/ci_ts_programs.py" <<'PY'
PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]
BUNDLES = [("cli", "pkg/cli/main.ts", "cli: Run the CLI")]
LISTED = [("cli", "pkg/lib", ("cli",), "cli runs what its bundle loads")]
PY
  replace "$root/.github/ci-filters.yaml" $'\nother:' \
    $'\ncli:\n  - \'pkg/cli/**\'\n  # BEGIN listed\n  # END listed\nother:'
}

echo "case: the write mode writes a listed block of the modules a bundle loads"
root=$(fixture bundle-write)
bundled "$root"
run_write "$root"
written=$(<"$root/.github/ci-filters.yaml")
check "exits 0" "$(is "$status" 0)"
check "says in the block's head that the filter's bundles load its files" \
  "$(contains "$written" '^  # bundles load. An edit by hand is refused by that check, which writes these.$')"
check "lists the module the bundle loads" "$(contains "$written" "^  - 'pkg/lib/runtime.ts'$")"
check "leaves out the file app's program alone compiles" "$(is "$(contains "$written" "pkg/lib/lib.ts")" false)"
check "leaves out the file nothing loads" "$(is "$(contains "$written" 'unused')" false)"
check "holds the bundle's files to the filter, node_modules and the path outside the repository dropped" \
  "$(contains "$out" '^bundle of pkg/cli/main.ts: 2 files, 0 uncovered (filter cli)$')"
run_check "$root"
check "the check passes over what it wrote" "$(is "$status" 0)"
check "counts the bundle in its summary" "$(contains "$out" 'and every file the entries its job runs load (1 bundles, 1 filters)')"

echo "case: a listed block lacking a module the bundle loads"
root=$(fixture bundle-missing)
bundled "$root"
run_write "$root"
printf 'export const more = 1;\n' > "$root/pkg/lib/more.ts"
printf 'pkg/lib/more.ts\n' >> "$root/pkg/cli/bundled"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the module and the bundles that load it" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter cli's listed block lacks 'pkg/lib/more.ts', which the bundles of filter cli load (cli runs what its bundle loads): the block is generated from filter cli.s bundles, so run \`.github/scripts/check-ts-filters.sh write\`")"
check "and that the filter does not select a module its bundle loads" \
  "$(contains "$out" '::error file=pkg/lib/more.ts::filter cli does not select pkg/lib/more.ts, which the bundle of pkg/cli/main.ts loads (cli: Run the CLI): the filter must select it, or the change that breaks that bundle starts no job')"

echo "case: a listed block holding a module the bundle does not load"
root=$(fixture bundle-extra)
bundled "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" "  - 'pkg/lib/runtime.ts'" $'  - \'pkg/lib/runtime.ts\'\n  - \'pkg/lib/unused.ts\''
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the module and the bundles that do not load it" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter cli's listed block has 'pkg/lib/unused.ts', which no bundle of filter cli loads, so it selects the job for a change no step of it sees: the block is generated from filter cli.s bundles, so run \`.github/scripts/check-ts-filters.sh write\`")"

# Bun names the outdir it was given when it reports on it, and that outdir is a scratch directory
# this run makes and removes: a refusal shows it as <scratch>, so it reads the same on every run
# and names no path that is gone by the time anyone reads it.
echo "case: a bundle that does not build"
root=$(fixture bundle-fails)
bundled "$root"
run_write "$root"
printf 'error: ModuleNotFound resolving "pkg/cli/main.ts" (entry point)\nerror: wrote nothing to @outdir@/main.js\n' \
  > "$root/pkg/cli/fails"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry, the command, its exit and what it printed" \
  "$(contains "$out" '::error file=pkg/cli/main.ts::`bun build pkg/cli/main.ts --target bun --outdir <scratch> --metafile=<scratch>/meta.json` failed (exit 1), so the files its bundle loads are unknown: error: / ModuleNotFound')"
check "shows the outdir Bun printed as <scratch>" "$(contains "$out" 'error: / wrote / nothing / to / <scratch>/main.js')"
check "and names no absolute path of the scratch directory" \
  "$(is "$(contains "$out" "$(<"$root/pkg/cli/outdir")")" false)"
check "counts that one problem, holding the block to no empty set" \
  "$(contains "$out" '^::error::1 problem(s) across 1 tsc program(s), 1 bundle(s) and 1 listed block(s)$')"
run_write "$root"
check "the write mode fails too" "$(is "$status" 1)"
check "and writes no block" "$(contains "$out" 'wrote no listed block: the files a block holds are computed from programs and bundles this run could not list, build or read')"
check "so the block keeps the module rather than going empty" \
  "$(contains "$(<"$root/.github/ci-filters.yaml")" "^  - 'pkg/lib/runtime.ts'$")"

# `bun build` is read from a regular file too: what it printed is only refusal text, but a pipe
# would cut the text a refusal quotes, as it cuts a tsc list, and Bun writes its report in pieces.
echo "case: a bundle that does not build, whose report a pipe would cut"
root=$(fixture bundle-fails-pipe-cut)
bundled "$root"
printf 'error: ModuleNotFound resolving "pkg/cli/main.ts" (entry point)\nerror: the half of the report a pipe loses\n' \
  > "$root/pkg/cli/fails"
: > "$root/pkg/cli/pipe-cut"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "quotes the whole report, the half a pipe loses included" \
  "$(contains "$out" 'error: / ModuleNotFound .* error: / the / half / of / the / report / a / pipe / loses')"

echo "case: a bundle that builds and leaves no metafile"
root=$(fixture bundle-no-metafile)
bundled "$root"
run_write "$root"
: > "$root/pkg/cli/nometa"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry, the command and the error, the scratch directory shown as <scratch>" \
  "$(contains "$out" "::error file=pkg/cli/main.ts::\`bun build pkg/cli/main.ts --target bun --outdir <scratch> --metafile=<scratch>/meta.json\` exited 0 and left no metafile whose \`inputs\` this check can read (FileNotFoundError: \[Errno 2\] No such file or directory: '<scratch>/meta.json'), so the files its bundle loads are unknown")"
check "and names no absolute path of the scratch directory" \
  "$(is "$(contains "$out" "$(<"$root/pkg/cli/outdir")")" false)"

# A bundle loads at least its entry, so a metafile naming no module of the repository is a
# metafile this check cannot trust, never a bundle of no files: a block computed from it would be
# empty, and the write mode would delete it.
echo "case: a bundle whose metafile names no module of the repository"
root=$(fixture bundle-no-inputs)
bundled "$root"
run_write "$root"
: > "$root/pkg/cli/bundled"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry and the command, and says its metafile names no module" \
  "$(contains "$out" "::error file=pkg/cli/main.ts::\`bun build pkg/cli/main.ts --target bun --outdir <scratch> --metafile=<scratch>/meta.json\` exited 0 and the \`inputs\` of its metafile name 0 module(s), none of them a file of this repository outside node_modules, so the files its bundle loads are unknown: a bundle loads at least its entry")"
check "counts that one problem, holding the block to no empty set" \
  "$(contains "$out" '^::error::1 problem(s) across 1 tsc program(s), 1 bundle(s) and 1 listed block(s)$')"
run_write "$root"
check "the write mode fails too" "$(is "$status" 1)"
check "and writes no block" "$(contains "$out" 'wrote no listed block: the files a block holds are computed from programs and bundles this run could not list, build or read')"
check "so the block keeps the module rather than going empty" \
  "$(contains "$(<"$root/.github/ci-filters.yaml")" "^  - 'pkg/lib/runtime.ts'$")"
printf 'node_modules/zod/index.js\n../outside/elsewhere.ts\n' > "$root/pkg/cli/bundled"
run_check "$root"
check "one naming only modules under node_modules and outside the repository fails the same way" \
  "$(contains "$out" "::error file=pkg/cli/main.ts::\`bun build pkg/cli/main.ts --target bun --outdir <scratch> --metafile=<scratch>/meta.json\` exited 0 and the \`inputs\` of its metafile name 2 module(s), none of them a file of this repository outside node_modules")"

echo "case: a bundle whose metafile names a module that is no file"
root=$(fixture bundle-nameless-input)
bundled "$root"
printf 'pkg/lib/gone.ts\n' >> "$root/pkg/cli/bundled"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the entry, the command and the module" \
  "$(contains "$out" "::error file=pkg/cli/main.ts::\`bun build pkg/cli/main.ts --target bun --outdir <scratch> --metafile=<scratch>/meta.json\` exited 0 and the \`inputs\` of its metafile hold 1 module(s) that name no file this check can find: 'pkg/lib/gone.ts', so the list is cut or holds a name that is no path, and the files its bundle loads are unknown")"
check "holds the filter to no list" "$(is "$(contains "$out" '^bundle of pkg/cli/main.ts: ')" false)"
check "counts that one problem" \
  "$(contains "$out" '^::error::1 problem(s) across 1 tsc program(s), 1 bundle(s) and 1 listed block(s)$')"

# union <root>: the bundled fixture with a second listed block, of the filter consumer, whose job
# loads what app's program compiles of pkg/lib and runs the CLI too, so LISTED takes its files
# from the two filters: pkg/lib/lib.ts from app's program, pkg/lib/runtime.ts from cli's bundle,
# and pkg/lib/unused.ts from neither. consumer is written before cli, so a replacement that ends
# at cli's key line edits consumer's block.
union() {
  local root=$1
  bundled "$root"
  cat > "$root/.github/scripts/ci_ts_programs.py" <<'PY'
PROGRAMS = [("app", "pkg/app", "app: Typecheck the app")]
BUNDLES = [("cli", "pkg/cli/main.ts", "cli: Run the CLI")]
LISTED = [
    ("cli", "pkg/lib", ("cli",), "cli runs what its bundle loads"),
    ("consumer", "pkg/lib", ("app", "cli"), "consumer loads what app compiles and runs the CLI"),
]
PY
  replace "$root/.github/ci-filters.yaml" $'\ncli:' \
    $'\nconsumer:\n  - \'pkg/app/**\'\n  # BEGIN listed\n  # END listed\ncli:'
}

# consumer_tail: the last lines of consumer's written block and the key line after it.
consumer_tail=$'  - \'pkg/lib/runtime.ts\'\n  # END listed\ncli:'

echo "case: a listed block of two sources holds the union of their files"
root=$(fixture union-write)
union "$root"
run_write "$root"
consumer=$(awk '/^consumer:$/ { on = 1; next } /^[a-z_]+:/ { on = 0 } on' "$root/.github/ci-filters.yaml")
check "exits 0" "$(is "$status" 0)"
check "names the first source on the head's first line" \
  "$(contains "$consumer" '^  # BEGIN listed by `.github/scripts/check-ts-filters.sh write`: the files of pkg/lib filter app.s$')"
check "and the second on its next" \
  "$(contains "$consumer" '^  # tsc programs and filter cli.s bundles load. An edit by hand is refused by that check, which writes these.$')"
check "lists the file app's program alone compiles" "$(contains "$consumer" "^  - 'pkg/lib/lib.ts'$")"
check "lists the module cli's bundle alone loads" "$(contains "$consumer" "^  - 'pkg/lib/runtime.ts'$")"
check "leaves out the file neither gives" "$(is "$(contains "$consumer" 'unused')" false)"
check "says what it wrote" \
  "$(contains "$out" '^filter consumer: wrote 2 listed file(s) under pkg/lib, from filter app.s tsc programs and filter cli.s bundles$')"
run_check "$root"
check "the check passes over what it wrote" "$(is "$status" 0)"

# Every refusal of a one-source block holds a block of two sources to the union of theirs.
echo "case: a listed block of two sources lacking a module one of them loads"
root=$(fixture union-missing)
union "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" "$consumer_tail" $'  # END listed\ncli:'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the module and the source that gives it" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer's listed block lacks 'pkg/lib/runtime.ts', which the bundles of filter cli load (consumer loads what app compiles and runs the CLI): the block is generated from filter app.s tsc programs and filter cli.s bundles, so run \`.github/scripts/check-ts-filters.sh write\`")"

echo "case: a listed block of two sources holding a file neither gives"
root=$(fixture union-extra)
union "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" "$consumer_tail" \
  $'  - \'pkg/lib/runtime.ts\'\n  - \'pkg/lib/unused.ts\'\n  # END listed\ncli:'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the file and both sources" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer's listed block has 'pkg/lib/unused.ts', which no tsc program of filter app compiles and no bundle of filter cli loads, so it selects the job for a change no step of it sees")"

echo "case: a listed block of two sources out of sorted order"
root=$(fixture union-unsorted)
union "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" $'  - \'pkg/lib/lib.ts\'\n'"$consumer_tail" \
  $'  - \'pkg/lib/runtime.ts\'\n  - \'pkg/lib/lib.ts\'\n  # END listed\ncli:'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter and the command that writes the block" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer's listed block holds its files out of sorted order or twice, and the same sources must write the same text on every run; run \`.github/scripts/check-ts-filters.sh write\`")"

echo "case: a pattern outside a listed block of two sources matching a file of its tree"
root=$(fixture union-loose)
union "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" $'\nconsumer:\n' $'\nconsumer:\n  - \'pkg/lib/**\'\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the pattern, a file of the tree it matches and both sources" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter consumer has 'pkg/lib/\*\*' outside its listed block, and it matches pkg/lib/lib.ts under pkg/lib: which files of pkg/lib the filter selects is the block's to say, from filter app.s tsc programs and filter cli.s bundles")"

echo "case: a file of a listed block of two sources that its filter does not select"
root=$(fixture union-unselected)
union "$root"
run_write "$root"
replace "$root/.github/ci-filters.yaml" $'\nconsumer:\n' $'\nconsumer:\n  - \'!pkg/lib/runtime.ts\'\n'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the file and the source that gives it" \
  "$(contains "$out" '::error file=pkg/lib/runtime.ts::filter consumer does not select pkg/lib/runtime.ts, which the bundles of filter cli load (consumer loads what app compiles and runs the CLI): the filter must select it, or a change to it starts no job')"

# The browser suite's rows spawn the dispatch command: packages/dispatch/e2e/dispatch-cli.ts:8,39
# runs `bun packages/envoy-client/bin/dispatch.ts` for the inbox-cli, asked-of-cli, asked-of and
# doc specs. So the suite loads the contracts modules that dispatch_ui's programs compile and that
# command's bundle loads, the bundle BUNDLES pairs with dispatch_two_builds_go.
# browser <root>: a fixture of that shape. dispatch_ui's program is app's,
# dispatch_two_builds_go's bundle loads pkg/lib/runtime.ts, which no program compiles, and
# dispatch_browser's block holds pkg/lib/lib.ts alone, which is that program's share of pkg/lib.
# The block's sources are the dispatch_browser row of the real ci_ts_programs.py, so the case
# holds that row and not a copy of it.
browser() {
  local root=$1 sources
  sources=$(cd "$script_dir" && PYTHONDONTWRITEBYTECODE=1 python3 -c \
    'from ci_ts_programs import LISTED; print(repr(next(row[2] for row in LISTED if row[0] == "dispatch_browser")))')
  mkdir -p "$root/pkg/cli"
  printf 'export const cli = 1;\n' > "$root/pkg/cli/main.ts"
  printf 'export const runtime = 1;\n' > "$root/pkg/lib/runtime.ts"
  printf 'pkg/cli/main.ts\npkg/lib/runtime.ts\n' > "$root/pkg/cli/bundled"
  cat > "$root/.github/scripts/ci_ts_programs.py" <<PY
PROGRAMS = [("dispatch_ui", "pkg/app", "dispatch-ui: Typecheck the dashboard")]
BUNDLES = [("dispatch_two_builds_go", "pkg/cli/main.ts", "dispatch-two-builds: Run the dispatch command")]
LISTED = [("dispatch_browser", "pkg/lib", $sources, "the browser suite runs the dashboard and spawns the dispatch command")]
PY
  replace "$root/.github/ci-filters.yaml" $'\napp:' $'\ndispatch_ui:'
  replace "$root/.github/ci-filters.yaml" $'\nother:' \
    $'\ndispatch_two_builds_go:\n  - *library\n  - \'pkg/cli/**\'\ndispatch_browser:\n  - \'pkg/app/**\'\n  # BEGIN listed by `.github/scripts/check-ts-filters.sh write`: the files of pkg/lib filter dispatch_ui\'s\n  # tsc programs compile. An edit by hand is refused by that check, which writes these.\n  - \'pkg/lib/lib.ts\'\n  # END listed\nother:'
}

echo "case: the browser suite's listed block lacking a module only the dispatch command's bundle loads"
root=$(fixture browser-bundle)
browser "$root"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the filter, the module and the bundle that loads it" \
  "$(contains "$out" "::error file=.github/ci-filters.yaml::filter dispatch_browser's listed block lacks 'pkg/lib/runtime.ts', which the bundles of filter dispatch_two_builds_go load (the browser suite runs the dashboard and spawns the dispatch command): the block is generated from filter dispatch_ui.s tsc programs and filter dispatch_two_builds_go.s bundles, so run \`.github/scripts/check-ts-filters.sh write\`")"
run_write "$root"
written=$(<"$root/.github/ci-filters.yaml")
check "the write mode passes" "$(is "$status" 0)"
check "and lists that module in the block" "$(contains "$written" "^  - 'pkg/lib/runtime.ts'$")"
check "under a head naming both sources" \
  "$(contains "$written" '^  # tsc programs and filter dispatch_two_builds_go.s bundles load. An edit by hand is refused by that check, which writes these.$')"
run_check "$root"
check "the check passes over what it wrote" "$(is "$status" 0)"

echo "case: a mode the script does not have"
root=$(fixture bad-mode)
set +e
out=$(PATH="$root/bin:$PATH" "$root/.github/scripts/check-ts-filters.sh" rewrite 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "names both modes" "$(contains "$out" 'takes one optional mode: `check` (the default)')"

summary "check-ts-filters.sh holds each Bun gate's filter to every file its tsc programs compile and its bundles load"
