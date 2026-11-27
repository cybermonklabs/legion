#!/usr/bin/env bash
# Writes Legion's CLI reference, <content dir>/legion/reference/cli.md, from the `legion` binary on
# PATH (docs/site/scripts/build-binaries.sh builds the Legion daemon's Go CLI and puts it there).
# `legion --help` lists the commands a person runs under "Available Commands:"; each command's
# `--help` is its section, and a command whose help lists commands of its own gets one section per
# command, from that command's own `--help`, at any depth. Nothing here names a command: a command
# added to the binary appears in the reference on the next build. cobra's own `help` and
# `completion` are left out, and so is every command only a program runs, which the binary hides
# from its help.
#
#   docs/site/generators/legion-cli.sh <content dir>
#
# Every help is run with a scrubbed environment (no LEGION_* variable, a scratch HOME, stdin from
# /dev/null), so no command can find a grant, a daemon or a workspace to act on. A help that exits
# with anything but 0 or holds no usage line for its own command (`  legion <command>…` under
# `Usage:`), or a `legion` whose --help lists no commands, fails the build: no page is published
# from a binary it cannot read, and none shows a panic or an error.
set -euo pipefail
# bash 5.2 reads `&` in a ${var//pattern/replacement} as the match; escape() needs it literal.
shopt -u patsub_replacement 2>/dev/null || true
# shellcheck source=SCRIPTDIR/lib/fence.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/fence.sh"

me=${0##*/}
fail() {
  echo "$me: $*" >&2
  exit 1
}

[ $# -eq 1 ] || {
  echo "usage: $me <content dir>" >&2
  exit 2
}
content=$1
[ -d "$content" ] || fail "$content is not a directory"
legion=$(command -v legion) || fail "no legion on PATH; build it first (docs/site/scripts/build-binaries.sh)"

scratch=$(mktemp -d "${TMPDIR:-/tmp}/legion-cli-reference.XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT

# help runs `legion <args…>` as nothing but a help reader and prints what it wrote, both streams.
# It must exit 0, and when its last argument is --help, print the usage of the command before it.
help() {
  local code=0 output path
  output=$(env -i PATH="$PATH" HOME="$scratch" LANG=C.UTF-8 timeout 30 "$legion" "$@" </dev/null 2>&1) || code=$?
  [ "$code" = 0 ] || fail "legion $* exited $code: $output"
  if [ "${*: -1}" = --help ]; then
    path="legion ${*:1:$#-1}"
    path=${path% }
    usage "$path" "$output" || fail "legion $* printed no usage of $path: $output"
  fi
  printf '%s\n' "$output"
}

# under prints the lines of <text> under the line <heading>, up to the first blank line.
under() {
  local heading=$1 text=$2 line found=0
  while IFS= read -r line; do
    if [ "$found" = 0 ]; then
      if [ "$line" = "$heading" ]; then found=1; fi
      continue
    fi
    [ -n "$line" ] || break
    printf '%s\n' "$line"
  done <<<"$text"
}

# usage says whether <help> holds a usage line of <path>: a line under `Usage:` that is <path>
# alone or <path> and more words.
usage() {
  local path=$1 line
  while IFS= read -r line; do
    case $line in
    "  $path" | "  $path "*) return 0 ;;
    esac
  done <<<"$(under Usage: "$2")"
  return 1
}

# commands prints, one per line and tab-separated, the name and summary of each command <help>
# lists under "Available Commands:", leaving out cobra's own help and completion. Its caller
# captures what it prints (`listing=$(commands …)`), so a line it cannot read stops the build.
commands() {
  local line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    [[ $line =~ ^\ \ ([a-z][a-z0-9-]*)\ +(.*)$ ]] || fail "unexpected line in a command list: $line"
    case ${BASH_REMATCH[1]} in
    help | completion) continue ;;
    esac
    printf '%s\t%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
  done <<<"$(under 'Available Commands:' "$1")"
}

# escape makes text safe in a Markdown paragraph or table cell: `<team>` would be read as HTML.
escape() {
  local text=$1
  text=${text//&/&amp;}
  text=${text//</&lt;}
  text=${text//>/&gt;}
  text=${text//|/\\|}
  printf '%s' "$text"
}

top=$(help --help)
grep -qx '  legion \[command\]' <<<"$top" ||
  fail "the legion on PATH ($legion) is not the Legion daemon's Go CLI: its --help has no 'legion [command]' usage line"

names=()
summaries=()
listing=$(commands "$top")
while IFS=$'\t' read -r name summary; do
  [ -n "$name" ] || continue
  names+=("$name")
  summaries+=("$summary")
done <<<"$listing"
[ "${#names[@]}" -gt 0 ] || fail "legion --help lists no commands"

out=$content/legion/reference
mkdir -p "$out"
page=$out/cli.md

# section writes the section of `legion <path…>` at heading <level>, then its commands' one level
# down. Each level's help lists the commands of the next, so the recursion ends where the binary's
# commands do.
section() {
  local level=$1
  shift
  local path="$*" text sub hashes ticks listing
  hashes=$(printf '%*s' "$level" '' | tr ' ' '#')
  text=$(help "$@" --help)
  ticks=$(fence <<<"$text")
  {
    printf '%s legion %s\n\n' "$hashes" "$path"
    printf '%stext\n%s\n%s\n\n' "$ticks" "$text" "$ticks"
  } >>"$page"
  listing=$(commands "$text")
  while IFS=$'\t' read -r sub _; do
    [ -n "$sub" ] || continue
    section $((level + 1)) "$@" "$sub"
  done <<<"$listing"
}

version=$(help version)
{
  cat <<'EOF'
---
title: CLI reference
description: Every legion command, subcommand and flag, generated from the binary's own help.
---

EOF
  # shellcheck disable=SC2016 # the backticks are Markdown code spans, not command substitutions
  printf 'Generated when the site was built, from `legion --help` and each command'"'"'s `--help` (`%s`). Run `legion <command> --help` for the same text from the binary you have, and `legion completion bash|zsh|fish|powershell` for a shell'"'"'s completion script.\n\n' "$version"
  printf '| Command | What it does |\n| --- | --- |\n'
  for i in "${!names[@]}"; do
    # shellcheck disable=SC2016 # the backticks are a Markdown code span, not a command substitution
    printf '| [`legion %s`](#legion-%s) | %s |\n' "${names[$i]}" "${names[$i]}" "$(escape "${summaries[$i]}")"
  done
  printf '\n'
} >"$page"
for name in "${names[@]}"; do
  section 2 "$name"
done

echo "$me: wrote $page (${#names[@]} commands, from $legion)"
