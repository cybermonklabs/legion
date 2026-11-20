#!/usr/bin/env bash
# Every Go module a required CI job builds is tidy: `go mod tidy -diff` changes nothing in it.
#
# A module whose go.mod lacks a requirement, or whose go.sum lacks a line, builds in CI today only
# because something else covers the gap: the workspace's other go.sum files and its go.work.sum
# hold the sums, and the other module's go.mod requires the module. A build that reads the module
# alone (GOWORK=off: packages/envoy's image, .github/scripts/build-envoy-command.sh, a program
# that requires the module) then has to fetch the missing module or its sum while it runs, or it
# fails under -mod=readonly naming the line it lacks; and a change elsewhere in the workspace, one
# that drops the requirement that covered the gap, does the same to every job. So the gap fails
# here, named, on the change that opens it.
#
# Which modules: the main modules a go command has in the directory of each `deps` and `tidy` line
# of the fetch files under .github/go-fetch/ (`go list -m` there: every module of the nearest
# go.work's use lines, or the one module holding that directory). Those files list what the
# required Go jobs' steps build: each job's fetch step runs its file, and rule 7 of
# .github/scripts/check-go-offline.sh refuses a go command whose packages no deps line of a fetch
# before it holds (or which it cannot read, unless a reason names the line that does), among
# those a required job's step runs in its own run text or hands to a shell there (bash -c, a
# pipe, a heredoc). A go command inside a script file or a function the step runs is not read
# by that rule; the job env's GOPROXY=off fails such a command at run time, naming the module its
# cache lacks, and rule 2 holds an offline build there to a fetch of its module. So a go.mod no
# such line reaches (a test's fixture under testdata, a research prototype under docs) is no
# module a required job's step builds in its own text, and is not read.
#
# Offline. This runs with GOPROXY=off, so it reads the module cache alone, and the cache must
# hold what tidy reads, which is more than any build: tidy reads every package any file of the
# module imports under any build tag and for any platform, the tests of each of those packages
# included, and the go.mod of every module the graph names (cmd/go's modcmd/tidy.go: all
# packages, imports.AnyTags, LoadTests). A job that runs this check fetches exactly that first,
# with one `tidy <module>` line of its fetch file per module (.github/scripts/go-fetch.sh), and a
# module the cache still lacks fails the module here with go's own line naming it.
# GOSUMDB=off too: with GOPROXY=off go asks sum.golang.org directly for the sum of a module its
# go.sum lacks, and every such sum is a line of the diff that fails the module anyway, printed and
# never written. A tidy module's go.sum holds every sum tidy reads, so it asks for none.
#
# Exits 0 when every module is tidy; 1 naming each module that is not, with tidy's diff or its
# error, and each fetch file line whose directory go cannot place in a module; 2 (UNKNOWN) when
# it finds no module to read, since a check that read nothing has passed nothing.
#
# Run from anywhere: .github/scripts/check-go-tidy.sh
# CI runs it in ci.yaml's text-checks job, after that job's fetch step.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export GOPROXY=off GOSUMDB=off

problems=0
declare -A seen=() dirs_by_input=()
files=0
shopt -s nullglob
for fetch in "$root"/.github/go-fetch/*.txt; do
  files=$((files + 1))
  n=0
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    read -ra words <<<"${line%%#*}"
    # The platform settings go-fetch.sh takes in front of `deps` choose no module.
    while [ "${#words[@]}" -gt 0 ] && [[ ${words[0]} == *=* ]]; do
      words=("${words[@]:1}")
    done
    [ "${#words[@]}" -ge 2 ] || continue
    case ${words[0]} in
      deps | tidy) ;;
      *) continue ;;
    esac
    where=${fetch#"$root"/}:$n
    input=${words[1]}
    if [ -n "${dirs_by_input[$input]:-}" ]; then
      dirs=${dirs_by_input[$input]}
      why=""
    elif [ ! -d "$root/$input" ]; then
      why="no such directory"
    elif ! dirs=$(cd "$root/$input" && go list -m -f '{{.Dir}}' 2>&1 </dev/null); then
      why=$dirs
    elif ! grep -q '^/' <<<"$dirs"; then
      # Outside every module and workspace, go list -m names command-line-arguments and no
      # directory, and exits 0.
      why="go list -m names no module directory there"
    else
      dirs_by_input[$input]=$dirs
      why=""
    fi
    if [ -n "$why" ]; then
      echo "::error file=${fetch#"$root"/},line=$n::$where runs ${words[0]} in $input, where go places no module: $why"
      problems=$((problems + 1))
      continue
    fi
    while IFS= read -r dir; do
      [ -n "$dir" ] || continue
      rel=${dir#"$root"/}
      seen[$rel]=1
    done <<<"$dirs"
  done <"$fetch"
done

if [ "${#seen[@]}" -eq 0 ]; then
  echo "UNKNOWN: check-go-tidy found no module to read: no deps or tidy line in the $files fetch files under .github/go-fetch/ places one"
  exit 2
fi

mapfile -t modules < <(printf '%s\n' "${!seen[@]}" | sort)
echo "check-go-tidy: ${#modules[@]} modules a required job builds (the deps and tidy lines of $files fetch files): ${modules[*]}"
untidy=0
for rel in "${modules[@]}"; do
  dir=$rel
  [[ $dir == /* ]] || dir=$root/$rel
  rc=0
  out=$(cd "$dir" && go mod tidy -diff 2>&1) || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "tidy: $rel"
    continue
  fi
  untidy=$((untidy + 1))
  if [[ $out == *"GOPROXY=off"* ]]; then
    echo "::error file=$rel/go.mod::$rel: go mod tidy -diff exits $rc, the module cache lacking a module tidy reads (go's line below names it): the job fetches what tidy reads with a \`tidy $rel\` line of its fetch file, before this check"
  else
    echo "::error file=$rel/go.mod::$rel is not tidy: go mod tidy -diff exits $rc. Run \`go mod tidy\` in $rel and commit its go.mod and go.sum. What tidy says:"
  fi
  printf '%s\n' "$out" | sed 's/^/    /'
done

if [ "$untidy" -gt 0 ] || [ "$problems" -gt 0 ]; then
  echo "::error::check-go-tidy: $untidy of the ${#modules[@]} modules are not tidy, and $problems fetch file lines place no module"
  exit 1
fi
echo "check-go-tidy: every one of the ${#modules[@]} modules is tidy"
