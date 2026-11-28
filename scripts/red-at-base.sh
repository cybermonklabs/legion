#!/usr/bin/env bash
# red-at-base.sh - Runs each test a change adds at an older commit, the base, and says whether the
# test fails there. A new test proves its change only when it fails without the change.
#
# usage: bash scripts/red-at-base.sh --base <commit> --list <file>
#
# How it works. The tool reads the whole list first, and refuses it (exit 2, no test run) when a
# line is malformed or names a path the checkout does not have. Then, for each line, it makes a git
# worktree of the base in a directory of its own (mktemp -d under TMPDIR), copies the line's files
# into it from the checkout it runs in, runs the line's test there, prints one line that says how
# the test ended at the base, and removes the worktree before the next line, so that each line runs
# on the base and its own files alone. The verdicts:
#
#   RED           the test ran and failed
#   BUILD-FAILED  the line's files do not build at the base, such as a test that calls code the base
#                 does not have, or an e2e driver that reads it
#   GREEN         the test ran and passed at the base, so it does not tell the base from the change
#   NO-TEST       no test the line names ran to a pass or a fail: no test matched, each match
#                 skipped, the run failed before a test started, or the base does not build there
#                 without the line's files, so that a failure would prove nothing about the test
#
# RED and BUILD-FAILED count as red. The tool exits 0 when every line is red, 1 when a line is not,
# and 2 when it refuses its arguments or the list, or cannot make a worktree.
#
# The list holds one test on each line, its fields separated by blanks (a regex writes a blank as
# \s):
#
#   <kind> <dir> <test regex or case> <file>...
#   cmd <dir> <name> [<file>...] -- <command> [<argument>...]
#
# Blank lines, and lines whose first field starts with #, are passed over; a list with no other line
# is refused, since a run over nothing proves nothing. Each path is relative to the top of the
# checkout. Each <file> is copied as the checkout's working tree holds it, committed or not, over the
# base's copy at the same path; a go, bun or e2e line names at least one, and a cmd line names any
# number.
#
#   go   <dir> is the package directory. The run is
#          go -C <dir> test -json -count=1 -run <regex> .
#        and the tool reads go's stream of events, in which each result is an event of its own,
#        apart from what a test prints: a test that prints "--- FAIL: X" and passes reads as passed.
#        Its reader of the stream is a Go program of its own (scripts/red-at-base/go-verdict), which
#        it builds once, before the first line runs, when the list holds a go line.
#        Before it copies the files, the tool asks go list which Go files the base builds in <dir>.
#        When the base builds one there, the tool builds the base package's test binary (go test
#        -c), and a base that does not build there reads NO-TEST, so that a build that fails after
#        the copy is the failure of the line's files. When it builds none, because <dir> is new or a
#        build constraint leaves out each of its files, the line's files are the whole package.
#        A build of the run that fails is BUILD-FAILED. Otherwise only the results of the tests the
#        regex selects count. The regex is read as the testing package reads it: split at each / and
#        | outside [] and (), into alternatives of one pattern for each level of a test's name, and
#        each pattern rewritten as the testing package rewrites names (each blank, U+00A0 among
#        them, becomes _, and an unprintable character its Go escape). A test is selected when its
#        name, split at each /, matches each pattern of one alternative down to that alternative's
#        depth. So ^TestA$/^b$|^TestC$ selects TestA/b, what runs under it, and TestC, and a TestA
#        that fails before b starts counts for nothing. A failed selected test is RED, and so is a
#        failed run that ended inside a selected test (a timeout, an os.Exit); a passed one is
#        GREEN; skips alone, no tests to run, or a run that failed before a selected test ran, are
#        NO-TEST.
#   bun  <dir> is the directory bun test runs in. Each of the line's files under <dir> whose name
#        bun reads as a test file (a.test.ts, a_spec.js and the like) runs on its own:
#          bun test --no-install ./<test file> -t <regex>
#        and the line reads the most red of its files' verdicts (RED, then BUILD-FAILED, then
#        GREEN, then NO-TEST); a bun line names at least one such file. A failed test is RED, and so
#        is a failed hook (beforeAll, afterAll and the like), which bun counts as a failed test
#        named (unnamed): a hook can hold the assertions of a test's postcondition, so its failure
#        is never set aside. So is any other failure bun counts in a file that loaded. A file that
#        does not load, such as one that imports a name the base does not export, so that no test
#        or hook of it ran, is BUILD-FAILED; an import of a package that is not installed is
#        NO-TEST. A pass with no failure is GREEN; skips or todos alone, and "matched 0 tests", are
#        NO-TEST.
#   e2e  <dir> is the e2e script, such as scripts/e2e/check-states.sh, and the third field is one
#        case, or several with commas between them. The run, at the top of the worktree, is
#          <NAME>_E2E_CASES=<cases> bash <script>
#        where NAME is the script's file name without .sh, in capitals, with each - as _:
#        scripts/e2e/check-states.sh reads CHECK_STATES_E2E_CASES. A case reports its start and its
#        result, as this repository's e2e scripts do: the line "== <case>" when it starts, then
#        "ok <case>" (or "ok <case> (<n>s)") when it passes, or "FAIL <case>: <why>". A FAIL of a
#        case of the line is RED, and so is a run that exits other than 0 after a case of the line
#        started and before its ok. A run in which each case of the line started and passed is
#        GREEN. A case that never started, or one that started and printed no ok in a run that
#        exits 0 (it skipped, or reported nothing), makes the line NO-TEST.
#        The driver. An e2e script of this repository runs its cases with a driver it builds
#        first, the e2e test binary of a Go package (go test -c -tags e2e), and a driver that does
#        not build ends the run before any case starts. So the tool builds that binary itself, in
#        each directory that holds a Go file of the line: before the copy, where the base builds a
#        Go file there with the tag e2e, it builds the base's own driver, and a base whose driver
#        does not build there without the line's files makes the line NO-TEST, as a go line's
#        does; after the copy, a driver that does not build is the failure of the line's files,
#        BUILD-FAILED, and the script does not run. That line names what the base lacks, as go's
#        errors name it (undefined: <name>, <type> has no field or method <name>, unknown field
#        <name> of <type>), and go's first error. A driver that builds runs the script, and its
#        cases decide. A line with no Go file has no driver to build, and only its cases decide.
#   cmd  a command: its words are what follows the field --, run as they are, with no shell, in
#        <dir> of the worktree, after the line's files are copied. <name> names the line and
#        nothing else. The command runs with no input and with TMPDIR set to an empty directory of
#        its own under the tool's, removed with the worktree, so that what it writes there (a work
#        directory a script defaults under TMPDIR, say) starts fresh for each line and is gone
#        after. A small Go launcher (scripts/red-at-base/command-launch) enters <dir> and starts the
#        command without a shell or shell options of its own. It preserves the caller's exported
#        BASHOPTS and SHELLOPTS, sets PWD and OLDPWD as entering <dir> requires, and resolves PATH
#        there, including relative entries. BUILD-FAILED means only that entering <dir> or starting
#        the command failed (a missing command, missing shebang interpreter, or no permission).
#        Every other run is the command's own: an exit of 0 is GREEN, and any other exit, 127
#        included, is RED, the run's last lines quoted. So `bash missing.sh` started, since bash
#        ran, and reads RED with bash's own words: a cmd line names among its files each script of
#        the change its command runs, or the base's copy runs, or none does. A cmd line reads no
#        NO-TEST, so a command that cannot tell the base from the change, such as one that fails for
#        want of its own setup, reads RED there: such a line proves something only when its command
#        fails at the base for the change's reason.
#
# What a run reads. Each run gets the tool's own environment, so a database URL, GOCACHE, GOTMPDIR
# and GOFLAGS reach the test as the caller set them. The tool needs no network: go runs with
# GOPROXY=off, so a module the module cache does not hold fails the run (NO-TEST) instead of a
# download, and bun runs with --no-install. GOWORK is unset, so go reads the base's own go.work.
# The tool's two Go programs, each a directory of scripts/red-at-base beside it (a module of their
# own, which no Go workspace of the repository lists), are built from a copy in the tool's own
# directory with the caller's GOCACHE, and with no GOFLAGS, no GOWORK and no switch of toolchain
# (GOTOOLCHAIN=local). A worktree of the base holds no installed packages (git ignores
# node_modules), so the tool mirrors each node_modules directory of the checkout into the worktree.
# A symlink is copied as the link it is: a relative link to a workspace package of the
# repository resolves to the base's copy of that package, and a link into an installed package
# resolves to the checkout's install. A scope directory (@name) and .bin are mirrored entry by
# entry. Any other directory, such as bun's store node_modules/.bun or a package a hoisted install
# put there, becomes a link to the checkout's; a dot directory other than .bin and .bun, a cache
# that a tool writes, is left out.
#
# What it writes. Nothing in the checkout's working tree: the worktree and each file of a run are
# in the tool's own directory under TMPDIR, which must not be inside the checkout. git worktree add
# writes the worktree's entry in the repository's git directory. After each line, and at any exit,
# the tool removes the worktree with git worktree remove --force, and then the entry by its own
# path once it has read that the entry still names this worktree. It does not run git worktree
# prune, which has no form for one entry and would also drop the entries of other checkouts whose
# directories are away. A trap does the removal at each exit; on INT or TERM it waits for the
# running test to end first.
#
# stdout holds one line for each line of the list, as that line ends:
#   <VERDICT> line <n>: <kind> <dir> <test> -- <what decided it>
# where <n> is the line's number in the list file. stderr holds each run's own output, and a count
# at the end.
# Save exported options before set changes SHELLOPTS; the tool's options are not the command's.
command_environment=()
[[ ${BASHOPTS@a} != *x* ]] || command_environment+=("BASHOPTS=$BASHOPTS")
[[ ${SHELLOPTS@a} != *x* ]] || command_environment+=("SHELLOPTS=$SHELLOPTS")

set -euo pipefail

# The tool's Go programs, beside this script (the header's "What a run reads").
programs_source="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/red-at-base"
readonly programs_source

usage() {
  cat <<'EOF'
usage: bash scripts/red-at-base.sh --base <commit> --list <file>

Runs each test of the list at the base commit, and prints RED, BUILD-FAILED, GREEN or NO-TEST for
each line. A line is <kind> <dir> <test regex or case> <file>..., its kind one of go, bun and e2e,
or cmd <dir> <name> [<file>...] -- <command> [<argument>...].
The header of this script says how each kind runs and how each verdict is read.
exit: 0 every line is red (RED or BUILD-FAILED), 1 a line is not, 2 a refused argument or list
EOF
}

refuse() {
  echo "red-at-base.sh: $*" >&2
  exit 2
}

base=''
list=''
while (($# > 0)); do
  case $1 in
  --base | --list)
    (($# >= 2)) || {
      usage >&2
      refuse "$1 needs a value"
    }
    if [[ $1 == --base ]]; then base=$2; else list=$2; fi
    shift 2
    ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    refuse "$1 is no argument of this tool"
    ;;
  esac
done
[[ -n $base ]] || {
  usage >&2
  refuse 'no --base: name the commit to run the tests at'
}
[[ -n $list ]] || {
  usage >&2
  refuse 'no --list: name the file of the tests to run'
}
[[ -f $list && -r $list ]] || refuse "the list $list is no readable file"
[[ $base != -* ]] || refuse "--base $base names no commit"

repo="$(git rev-parse --show-toplevel 2>/dev/null)" || refuse 'the current directory is in no git checkout'
readonly repo
base_commit="$(git -C "$repo" rev-parse --verify --quiet "${base}^{commit}")" ||
  refuse "--base $base names no commit of this checkout"
readonly base_commit

# safe_path <line number> <path>: a path relative to the top of the checkout that stays in it.
safe_path() {
  [[ -n $2 && $2 != /* && /$2/ != */../* ]] || refuse "line $1 of $list: $2 is no path inside the checkout"
}

# ---- the list, read and checked whole before any test runs --------------------------------------
line_numbers=()
kinds=()
dirs=()
tests=()
line_files=()
bun_tests=()
commands=()
number=0
while IFS= read -r raw || [[ -n $raw ]]; do
  number=$((number + 1))
  read -r -a field <<<"$raw"
  ((${#field[@]} > 0)) || continue
  [[ ${field[0]} != \#* ]] || continue
  ((${#field[@]} >= 4)) ||
    refuse "line $number of $list has ${#field[@]} fields; a line is <kind> <dir> <test regex or case> <file>..."
  kind=${field[0]}
  dir=${field[1]%/}
  test=${field[2]}
  files=("${field[@]:3}")
  command_words=()
  if [[ $kind == cmd ]]; then
    [[ $test != -- ]] || refuse "line $number of $list: a cmd line names itself before its field --"
    # The files run up to the field --, and the command is what follows it.
    files=()
    for ((at = 3; at < ${#field[@]}; at++)); do
      [[ ${field[at]} != -- ]] || break
      files+=("${field[at]}")
    done
    ((at < ${#field[@]})) ||
      refuse "line $number of $list: a cmd line is cmd <dir> <name> [<file>...] -- <command> [<argument>...], and this one has no field --"
    command_words=("${field[@]:at+1}")
    ((${#command_words[@]} > 0)) || refuse "line $number of $list: nothing follows the field --, so the line names no command"
  fi
  safe_path "$number" "$dir"
  for path in "${files[@]}"; do
    safe_path "$number" "$path"
    [[ -f ${repo}/${path} ]] || refuse "line $number of $list: $path is no file of the checkout"
  done
  case $kind in
  go | bun | cmd)
    [[ -d ${repo}/${dir} ]] || refuse "line $number of $list: $dir is no directory of the checkout"
    ;;
  e2e)
    [[ $dir == *.sh && -f ${repo}/${dir} ]] ||
      refuse "line $number of $list: $dir is no e2e script (a .sh file) of the checkout"
    [[ $test =~ ^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$ ]] ||
      refuse "line $number of $list: $test is no case name, nor several with commas between them"
    ;;
  *)
    refuse "line $number of $list: $kind is no kind; a kind is go, bun, e2e or cmd"
    ;;
  esac
  bun_files=''
  if [[ $kind == bun ]]; then
    for path in "${files[@]}"; do
      relative=$path
      if [[ $dir != . ]]; then
        [[ $path == "$dir"/* ]] || continue
        relative=${path#"$dir"/}
      fi
      case ${relative##*/} in
      *.test.* | *_test.* | *.spec.* | *_spec.*)
        case ${relative##*.} in
        ts | tsx | js | jsx | mjs | cjs | mts | cts) bun_files+=" ./${relative}" ;;
        esac
        ;;
      esac
    done
    [[ -n $bun_files ]] ||
      refuse "line $number of $list: no file of the line under $dir is a test file bun runs (such as a.test.ts)"
  fi
  line_numbers+=("$number")
  kinds+=("$kind")
  dirs+=("$dir")
  tests+=("$test")
  line_files+=("${files[*]}")
  bun_tests+=("${bun_files# }")
  commands+=("${command_words[*]}")
done <"$list"
((${#kinds[@]} > 0)) || refuse "the list $list holds no line to run, and a run over nothing proves nothing"

# ---- the tool's own directory, and its removal at every exit -------------------------------------
tmp="$(mktemp -d "${TMPDIR:-/tmp}/red-at-base.XXXXXX")"
readonly tmp
case "$(realpath "$tmp")/" in
"$(realpath "$repo")"/*)
  rmdir -- "$tmp"
  refuse "TMPDIR (${TMPDIR:-/tmp}) is inside the checkout, and the tool writes nothing there"
  ;;
esac
readonly wt="${tmp}/base"
readonly log="${tmp}/run.log"
# The worktree's entry in the repository's git directory, and what its gitdir file held when the
# tool made it: the entry is removed by this path, and only while it still names this worktree.
admin=''
admin_gitdir=''
wt_real=''

drop_worktree() {
  if [[ -e $wt ]]; then
    git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1 || true
  fi
  if [[ -e $wt ]]; then
    chmod -R u+w -- "$wt" 2>/dev/null || true
    rm -rf -- "$wt"
  fi
  if [[ -n $admin && -f ${admin}/gitdir && "$(<"${admin}/gitdir")" == "$admin_gitdir" ]]; then
    rm -rf -- "$admin"
    # git removes the directory of the entries when its last entry goes; rmdir removes it only empty.
    rmdir -- "$(dirname -- "$admin")" 2>/dev/null || true
  fi
  admin=''
}

# shellcheck disable=SC2329 # the EXIT trap calls it
cleanup() {
  drop_worktree
  rm -rf -- "$tmp"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

make_worktree() {
  git -C "$repo" worktree add --detach --quiet "$wt" "$base_commit" >&2 ||
    refuse "git worktree add of $base_commit at $wt failed"
  admin="$(git -C "$wt" rev-parse --absolute-git-dir)"
  admin_gitdir="$(<"${admin}/gitdir")"
  wt_real="$(realpath "$wt")"
}

# inside <path>: true when the path, each symlink of it that exists resolved, is in the worktree.
inside() {
  local resolved
  resolved="$(realpath -m -- "$1")"
  [[ $resolved == "$wt_real" || $resolved == "$wt_real"/* ]]
}

# copy_files <line number> <path>...: each file of the line, from the checkout over the base's copy.
copy_files() {
  local number=$1 path
  shift
  for path in "$@"; do
    inside "${wt}/${path}" ||
      refuse "line $number of $list: $path leads out of the worktree of the base, through a symlink of the base"
    mkdir -p -- "$(dirname -- "${wt}/${path}")"
    cp -p -- "${repo}/${path}" "${wt}/${path}"
    echo "   copied $path" >&2
  done
}

# mirror_dir <from> <to>: one node_modules directory of the checkout, as the header's "What a run
# reads" says.
mirror_dir() {
  local from=$1 to=$2 entry name
  mkdir -- "$to"
  for entry in "$from"/* "$from"/.[!.]* "$from"/..?*; do
    [[ -e $entry || -L $entry ]] || continue
    name=${entry##*/}
    if [[ -L $entry ]]; then
      cp -P -- "$entry" "${to}/${name}"
    elif [[ -d $entry && ($name == @* || $name == .bin) ]]; then
      mirror_dir "$entry" "${to}/${name}"
    elif [[ -d $entry && ($name != .* || $name == .bun) ]]; then
      ln -s -- "$entry" "${to}/${name}"
    elif [[ -f $entry ]]; then
      cp -p -- "$entry" "${to}/${name}"
    fi
  done
}

# mirror_installed_packages: the node_modules directory beside each package.json the checkout
# tracks, wherever the base has that directory and no node_modules of its own there.
mirror_installed_packages() {
  local manifest directory
  while IFS= read -r -d '' manifest; do
    directory="$(dirname -- "$manifest")"
    [[ -d ${repo}/${directory}/node_modules && -d ${wt}/${directory} ]] || continue
    [[ ! -e ${wt}/${directory}/node_modules && ! -L ${wt}/${directory}/node_modules ]] || continue
    inside "${wt}/${directory}/node_modules" || continue
    mirror_dir "${repo}/${directory}/node_modules" "${wt}/${directory}/node_modules"
  done < <(git -C "$repo" ls-files -z -- package.json '*/package.json')
}

status=0
# run_in <directory> <command>...: the command, in the directory and with no input, its output in
# the run's log and its exit status in status. The output goes to a file and not through a pipe,
# so that a process a test leaves behind cannot hold the tool.
run_in() {
  local directory=$1
  shift
  {
    printf '   run in %s:' "$directory"
    printf ' %q' "$@"
    echo
  } >&2
  status=0
  (cd -- "$directory" && exec "$@") </dev/null >"$log" 2>&1 || status=$?
}

# run_logged <directory> <command>...: run_in, and then the run's log and exit status on stderr.
run_logged() {
  run_in "$@"
  cat -- "$log" >&2
  echo "   exit $status" >&2
}

# first_error: the run's first compiler error line, or else its first line.
first_error() {
  awk '/^[^ \t#].*:[0-9]+:[0-9]+: / { print; found = 1; exit }
    first == "" && NF { first = $0 }
    END { if (!found) print first }' "$log"
}

# The tool's Go programs, each built the first time a line needs it, into the tool's own directory
# from a copy of their module there: go-verdict, the reader of go's stream, and command-launch, the
# launcher of a cmd line's command, as the header says.
readonly programs="${tmp}/programs"
readonly go_reader="${programs}/bin/go-verdict"
readonly command_launcher="${programs}/bin/command-launch"
# build_program <name> <what it is>: the program scripts/red-at-base/<name>, built as the header's
# "What a run reads" says.
build_program() {
  [[ -d $programs ]] || cp -R -- "$programs_source" "$programs" ||
    refuse "the tool could not copy its Go programs from $programs_source"
  run_logged "$programs" env -u GOWORK -u GOFLAGS GOTOOLCHAIN=local GOPROXY=off go build -o "${programs}/bin/$1" "./$1"
  ((status == 0)) || refuse "the tool could not build its $2 (its build is above)"
}

# check_base_package <dir> [<go build flag>...]: result set to NO-TEST when the base's own package
# in dir builds a Go file, with the flags given, and does not build its test binary with them. A
# base that builds no Go file there, because dir is new or a build constraint leaves each file out,
# has no package of its own to break. A go line gives no flag, and an e2e line's driver -tags e2e.
check_base_package() {
  local dir=$1 files with=''
  shift
  (($# == 0)) || with=" with $*"
  [[ -d ${wt}/${dir} ]] || return 0
  run_logged "$wt" env -u GOWORK GOPROXY=off go -C "${wt}/${dir}" list -e "$@" \
    -f '{{len .GoFiles}} {{len .CgoFiles}} {{len .TestGoFiles}} {{len .XTestGoFiles}}' .
  files="$(awk '/^[0-9]+ [0-9]+ [0-9]+ [0-9]+$/ { print $1 + $2 + $3 + $4; exit }' "$log")"
  if ((status == 0)) && [[ $files == 0 ]]; then
    echo "   the base builds no Go file in $dir$with, so the line's files are the package there" >&2
    return 0
  fi
  run_logged "$wt" env -u GOWORK GOPROXY=off go -C "${wt}/${dir}" test -c "$@" -o /dev/null .
  ((status == 0)) ||
    result="NO-TEST"$'\t'"the base does not build in $dir$with without the line's files, so no failure there would be the test's: $(first_error)"
}

# e2e_packages <file>...: each directory that holds a Go file of the line, once each and in the
# line's order: the packages whose e2e driver an e2e line builds (the header's "The driver").
e2e_packages() {
  local path package seen=$'\n'
  for path in "$@"; do
    [[ $path == *.go ]] || continue
    package="$(dirname -- "$path")"
    [[ $seen != *$'\n'"$package"$'\n'* ]] || continue
    seen+="$package"$'\n'
    printf '%s\n' "$package"
  done
}

# missing_symbols: what the build's errors say the base lacks, each once, in go's order:
# "undefined: <name>"; "<type> has no field or method <name>", as <type>.<name>; and "unknown field
# <name> in struct literal of type <type>", as <type>.<name>. A type is written as the last element
# of its package's path and its name.
missing_symbols() {
  awk '
    function short(type, n, part) {
      gsub(/"/, "", type)
      sub(/^\*/, "", type)
      n = split(type, part, "/")
      return part[n]
    }
    function add(name) {
      if (name in seen) return
      seen[name] = 1
      out = out (out == "" ? "" : ", ") name
    }
    /: undefined: / {
      name = $0; sub(/.*: undefined: /, "", name); sub(/[ \t].*/, "", name)
      add(name)
    }
    / has no field or method / {
      type = $0; sub(/.*\(type /, "", type); sub(/ has no field or method .*/, "", type)
      name = $0; sub(/.* has no field or method /, "", name); sub(/[^A-Za-z0-9_].*/, "", name)
      add(short(type) "." name)
    }
    /unknown field .* in struct literal of type / {
      name = $0; sub(/.*unknown field /, "", name); sub(/ in struct literal.*/, "", name)
      type = $0; sub(/.* in struct literal of type /, "", type); sub(/[ ,)].*/, "", type)
      add(short(type) "." name)
    }
    END { print out }' "$log"
}

# check_line_driver <dir>: result set to BUILD-FAILED when the e2e driver in dir, the package's test
# binary with the tag e2e, does not build with the line's files copied in; every error is printed
# (-gcflags=-e), so that the line names all the base lacks.
check_line_driver() {
  local dir=$1 lacks
  run_logged "$wt" env -u GOWORK GOPROXY=off go -C "${wt}/${dir}" test -c -tags e2e -gcflags=-e -o /dev/null .
  ((status != 0)) || return 0
  lacks="$(missing_symbols)"
  ((${#lacks} <= 600)) || lacks="${lacks:0:600}..."
  result="BUILD-FAILED"$'\t'"the e2e driver in $dir (go test -c -tags e2e) does not build with the line's files${lacks:+; what the base lacks: $lacks}: $(first_error)"
}

# bun_verdict <exit status>: "<VERDICT><tab><what decided it>" for a bun test run of one file, read
# from bun's summary. A failed test counts once in "fail"; a file that does not load, once in "fail"
# and once in "error"; a failed hook, once in "fail", as a test named (unnamed). Every failure but a
# file that does not load is RED: a hook may hold the assertions of a test's postcondition.
bun_verdict() {
  RUN_STATUS=$1 awk '
    BEGIN { status = ENVIRON["RUN_STATUS"] + 0 }
    NF == 2 && $1 ~ /^[0-9]+$/ && $2 ~ /^(pass|fail|skip|todo|error|errors)$/ {
      count[$2 == "errors" ? "error" : $2] = $1 + 0
    }
    /matched 0 tests|had no matches/ && nomatch == "" { nomatch = $0 }
    /Cannot find package/ && missing == "" { missing = $0 }
    /^\(fail\) \(unnamed\)/ { hooks++; if (hook == "") hook = error_line; next }
    /^\(fail\) / && fail == "" { fail = $0 }
    /^(error|[A-Za-z]*Error): / { error_line = $0; if (error == "") error = $0 }
    first == "" && NF && !/^bun test v/ { first = $0 }
    END {
      named = count["fail"] - count["error"] - hooks
      if (nomatch != "") { print "NO-TEST\t" nomatch; exit }
      if (missing != "") { print "NO-TEST\ta package the test imports is not installed for the base: " missing; exit }
      if (count["error"] > 0 && count["pass"] + count["skip"] + count["todo"] + hooks + (named > 0 ? named : 0) == 0) {
        print "BUILD-FAILED\tthe file does not load at the base: " (error != "" ? error : "an error outside any test")
        exit
      }
      if (named > 0) { print "RED\t" (fail != "" ? fail : named " fail"); exit }
      if (hooks > 0) {
        print "RED\ta hook failed, which bun counts as a failed test named (unnamed): " (hook != "" ? hook : "no error line")
        exit
      }
      if (count["fail"] > 0) { print "RED\ta failure outside any test, in a file that loaded: " (error != "" ? error : first); exit }
      if (status != 0) { print "NO-TEST\tbun test failed with no test result (exit " status "): " (error != "" ? error : first); exit }
      if (count["pass"] > 0) { print "GREEN\t" count["pass"] " pass"; exit }
      print "NO-TEST\tno test the regex names ran to a pass or a fail (" count["skip"] + 0 " skip, " count["todo"] + 0 " todo)"
    }' "$log"
}

# most_red <verdict> <verdict>: the more red of two "<VERDICT><tab><why>" lines, the first of two
# alike; an empty first one is no verdict yet.
most_red() {
  local rank_of_first rank_of_second
  rank_of_first="$(verdict_rank "$1")"
  rank_of_second="$(verdict_rank "$2")"
  if ((rank_of_second > rank_of_first)); then printf '%s' "$2"; else printf '%s' "$1"; fi
}
verdict_rank() {
  case ${1%%$'\t'*} in
  RED) echo 4 ;;
  BUILD-FAILED) echo 3 ;;
  GREEN) echo 2 ;;
  NO-TEST) echo 1 ;;
  *) echo 0 ;;
  esac
}

# e2e_verdict <cases> <exit status>: "<VERDICT><tab><what decided it>" for a run of an e2e script,
# read by each case's start line and result line.
e2e_verdict() {
  RUN_CASES=$1 RUN_STATUS=$2 awk '
    function says(word, c) { return $0 == word " " c || index($0, word " " c " ") == 1 }
    BEGIN { n = split(ENVIRON["RUN_CASES"], want, ","); status = ENVIRON["RUN_STATUS"] + 0 }
    {
      for (i = 1; i <= n; i++) {
        if (says("==", want[i])) began[i] = 1
        else if (says("ok", want[i])) passed[i] = 1
        else if (!(i in failed) && (says("FAIL", want[i]) || index($0, "FAIL " want[i] ":") == 1)) failed[i] = $0
      }
    }
    END {
      for (i = 1; i <= n; i++) if (i in failed) { print "RED\t" failed[i]; exit }
      if (status != 0)
        for (i = 1; i <= n; i++)
          if ((i in began) && !(i in passed)) { print "RED\texit " status " after == " want[i] ", with no ok " want[i]; exit }
      for (i = 1; i <= n; i++) {
        if (!(i in began)) never = never (never == "" ? "" : ",") want[i]
        else if (!(i in passed)) silent = silent (silent == "" ? "" : ",") want[i]
        else started++
      }
      if (never == "" && silent == "") {
        print "GREEN\tok " ENVIRON["RUN_CASES"] (status != 0 ? ", and then the script exited " status : "")
        exit
      }
      if (silent != "") { print "NO-TEST\t" silent " started and printed no ok: it skipped, or reported no result (exit " status ")"; exit }
      if (started) { print "NO-TEST\tthe script never started " never " (exit " status ")"; exit }
      print "NO-TEST\tno line == <case> for " ENVIRON["RUN_CASES"] ": the script started no case of the line (exit " status ")"
    }' "$log"
}

# remove_tree <directory>: the directory and all it holds, made writable first, since a command may
# leave a directory it made read-only, as go does its module cache.
remove_tree() {
  [[ -e $1 ]] || return 0
  chmod -R u+w -- "$1" 2>/dev/null || true
  rm -rf -- "$1"
}

readonly command_tmp="${tmp}/cmd-tmp"
readonly start_mark="${tmp}/cmd-start"

start_failure=''
# run_command <directory> <command>...: the command of a cmd line, its output in the run's log
# and its exit status in status. A nonempty start record describes a failure to start it.
run_command() {
  local directory=$1
  shift
  rm -f -- "$start_mark"
  remove_tree "$command_tmp"
  mkdir -- "$command_tmp"
  {
    printf '   run in %s, with TMPDIR %s:' "$directory" "$command_tmp"
    printf ' %q' "$@"
    echo
  } >&2
  status=0
  TMPDIR="$command_tmp" env "${command_environment[@]}" "$command_launcher" "$start_mark" "$directory" "$@" \
    </dev/null >"$log" 2>&1 || status=$?
  [[ -f $start_mark ]] || refuse "the command launcher recorded no start outcome (exit $status): $(last_lines)"
  start_failure="$(<"$start_mark")"
  cat -- "$log" >&2
  echo "   exit $status" >&2
}

# last_lines: the run's last three lines that hold anything, with " | " between them.
last_lines() {
  awk 'NF { sub(/^[ \t]+/, ""); line[++n] = $0 }
    END { for (i = (n > 3 ? n - 2 : 1); i <= n; i++) printf "%s%s", (i > (n > 3 ? n - 2 : 1) ? " | " : ""), line[i] }' "$log"
}

# command_verdict <command> <exit status>: "<VERDICT><tab><what decided it>" for a cmd line.
command_verdict() {
  local last
  last="$(last_lines)"
  ((${#last} <= 600)) || last="...${last: -600}"
  if [[ -n $start_failure ]]; then
    printf 'BUILD-FAILED\tthe command never started: %s%s' "$start_failure" "${last:+: $last}"
  elif (($2 == 0)); then
    printf 'GREEN\texit 0 from %s%s' "$1" "${last:+: $last}"
  else
    printf 'RED\texit %s from %s%s' "$2" "$1" "${last:+: $last}"
  fi
}

# ---- each line, at the base ---------------------------------------------------------------------------
echo "red-at-base.sh: ${#kinds[@]} lines of $list, at the base $base ($base_commit), from the checkout $repo" >&2
for kind in "${kinds[@]}"; do
  case $kind in
  go) [[ -x $go_reader ]] || build_program go-verdict "reader of go test's stream" ;;
  cmd) [[ -x $command_launcher ]] || build_program command-launch "command launcher" ;;
  esac
done
red=0
build_failed=0
green=0
no_test=0
for i in "${!kinds[@]}"; do
  number=${line_numbers[i]}
  kind=${kinds[i]}
  dir=${dirs[i]}
  test=${tests[i]}
  read -r -a files <<<"${line_files[i]}"
  {
    echo
    echo "== line $number: $kind $dir $test ${line_files[i]}"
  } >&2
  make_worktree
  result=''
  drivers=()
  if [[ $kind == go ]]; then
    check_base_package "$dir"
  elif [[ $kind == e2e ]]; then
    mapfile -t drivers < <(e2e_packages "${files[@]}")
    for driver in "${drivers[@]}"; do
      [[ -z $result ]] || break
      check_base_package "$driver" -tags e2e
    done
  fi
  if [[ -z $result ]]; then
    copy_files "$number" "${files[@]}"
    mirror_installed_packages
    case $kind in
    go)
      run_in "$wt" env -u GOWORK GOPROXY=off go -C "${wt}/${dir}" test -json -count=1 -run "$test" .
      # The reader writes the run's text to stderr, and its verdict to stdout.
      result="$("$go_reader" "$test" "$status" <"$log")" || result=''
      echo "   exit $status" >&2
      ;;
    bun)
      read -r -a bun_files <<<"${bun_tests[i]}"
      for bun_file in "${bun_files[@]}"; do
        run_logged "${wt}/${dir}" bun test --no-install "$bun_file" -t "$test"
        file_result="$(bun_verdict "$status")"
        if ((${#bun_files[@]} > 1)); then
          file_result="${file_result%%$'\t'*}"$'\t'"${dir}/${bun_file#./}: ${file_result#*$'\t'}"
        fi
        result="$(most_red "$result" "$file_result")"
      done
      ;;
    e2e)
      for driver in "${drivers[@]}"; do
        [[ -z $result ]] || break
        check_line_driver "$driver"
      done
      if [[ -z $result ]]; then
        name=${dir##*/}
        name=${name%.sh}
        name=${name^^}
        run_logged "$wt" env "${name//[^A-Z0-9]/_}_E2E_CASES=${test}" bash "$dir"
        result="$(e2e_verdict "$test" "$status")"
      fi
      ;;
    cmd)
      read -r -a command_line <<<"${commands[i]}"
      run_command "${wt}/${dir}" "${command_line[@]}"
      result="$(command_verdict "${commands[i]}" "$status")"
      ;;
    esac
  fi
  drop_worktree
  remove_tree "$command_tmp"
  [[ -n $result ]] || result="NO-TEST"$'\t'"the tool could not read the run's log"
  verdict=${result%%$'\t'*}
  case $verdict in
  RED) red=$((red + 1)) ;;
  BUILD-FAILED) build_failed=$((build_failed + 1)) ;;
  GREEN) green=$((green + 1)) ;;
  *) no_test=$((no_test + 1)) ;;
  esac
  printf '%s line %s: %s %s %s -- %s\n' "$verdict" "$number" "$kind" "$dir" "$test" "${result#*$'\t'}"
done

{
  echo
  echo "red-at-base.sh: $((red + build_failed)) of ${#kinds[@]} lines are red at $base_commit" \
    "(RED $red, BUILD-FAILED $build_failed, GREEN $green, NO-TEST $no_test)"
} >&2
if ((red + build_failed == ${#kinds[@]})); then
  exit 0
fi
exit 1
