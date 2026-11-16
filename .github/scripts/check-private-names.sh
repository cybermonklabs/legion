#!/usr/bin/env bash
# This repository is public. The private repository Legion is deployed from, its private internal
# hosts, and the company that runs it are named nowhere in docs, skills, role prompts, CHANGELOGs,
# code comments, tests, fixtures or file names, since the published site renders text from all of
# them. This check fails on any text line naming one, and on any file path naming the first two or
# a term of the fifth rule:
#
#   1. the deployment repository — "agent", then "c", with nothing or one separator between them,
#      in any case, standing as a token of its own: no letter or digit on either side, and a
#      percent-encoded byte before it counts as a separator too. A separator is a "-" or "_", a
#      run of spaces or no-break spaces, a typographic dash, or a percent-encoded byte. That is the
#      repository's name, its Dispatch project key and every issue key under it, and the name
#      inside a session id between underscores, but not a camelCase or snake_case identifier that
#      merely starts with those letters (agentCursor, AgentConversation, subagentContext,
#      AGENT_COMPOSER_SELECTOR, agent-composer);
#   2. the company — its two words joined, or split by one separator or a ".", in any case (its
#      hostnames included); and its first word capitalized or in capitals, as a whole word or
#      before a "_", which is the company's proper noun or a constant named after it wherever it
#      stands, at the start of a sentence or a heading too. The lowercase English word names nothing
#      and passes;
#   3. a private host below `.internal.`, in any case. A reserved example host (`<name>` then
#      `.internal.example`, optionally `.com`, `.net` or `.org`) passes, and so does the NATS
#      client's own `this` property of that name in the one bundle that vendors it
#      (packages/claude-envoy/dist/envoy-channel.js, which reaches this check on main and through
#      packages/claude-envoy/scripts/verify-build.sh, never in a pull request's diff), and nowhere
#      else;
#   4. a Tailscale MagicDNS host — a DNS label then `.ts.net`, in any case, with no letter or
#      digit after it. A reserved tailnet name (`example-tailnet`, under any machine name of its
#      own in front) passes;
#   5. a term of PRIVATE_NAME_TERMS: the names this public file cannot spell, such as the
#      deployment repository's own name with its owner, the company's other forms and the cloud
#      project ids, kept in the Actions secret of that name. The variable holds one term a line,
#      each with the blanks around it (a carriage return among them) dropped, and a line left
#      empty is dropped too, since grep reads an empty pattern as one that matches every line.
#      Each term is matched as a fixed string, in any case, anywhere in a line or a path, so a term
#      that holds a "/" (an owner/repository form) is found in a path as well.
#
# Every CI job that runs this script, itself or through check-pr-text.sh or
# packages/claude-envoy/scripts/verify-build.sh, maps PRIVATE_NAME_TERMS from that secret into the
# step's env. With the variable unset or empty (no term left after the dropping above) the fifth
# rule cannot run, and GITHUB_EVENT_NAME decides what follows. On a pull_request or
# pull_request_review event, which GitHub runs with no secret for a fork's pull request, and with
# no event at all (a run outside Actions), the check prints a warning that only the first four
# rules ran and keeps their exit. On any other event (a merge_group, a push, a workflow_dispatch),
# where a job can always map the secret, it fails with exit 2 before it scans anything.
#
# With no arguments it scans the repository: every file git tracks, a file under an ignored path
# included, plus every untracked file git does not ignore, so a new file is caught before it is
# added. With arguments it scans each named directory or file instead, tracked or not; ci.yaml's
# docs job passes it the built site (docs/site/dist), whose generated reference pages and copied
# media captions exist only after the build, and check-pr-text.sh a pull request's title, body,
# branch name, commits (each one's message, author and committer), issue comments, reviews and
# review comments. The first two rules and the fifth also read every file's path, directories
# included, as git and find print it unquoted. Binary files are
# skipped, and so are lockfiles (bun.lock, go.sum, go.work.sum), whose integrity hashes are random
# text. Every hit is printed as its file and line, never its text, and a path component a rule
# matches is printed as <name> (a term that spans components is replaced where it stands), so the
# log does not repeat the name; the only non-zero exits are named hits (1), and a target or search
# that failed or a term list the event requires and the job did not map (2).
#
# The rules below are spelled so that no line of this file is refused by them: a line a rule's raw
# pattern matches, such as a reserved placeholder host, always clears that rule's own allow-list
# too, which is why the check needs no exception for itself.
#
# Run from anywhere: .github/scripts/check-private-names.sh [path...]
# CI runs it over the repository in ci.yaml's lint job; over the built site in docs-site.yaml,
# which ci.yaml's docs job and docs-pages.yaml's build job call; over the built Claude Code plugin
# through packages/claude-envoy/scripts/verify-build.sh, in ci.yaml's test and claude-envoy jobs
# and release.yaml's claude_envoy_build; over a pull request's title, body, branch name, commits
# (each one's message, author and committer), issue comments, reviews and review comments through
# check-pr-text.sh (ci.yaml's pr-title job at each push to a pull request that is not a draft, when
# a draft is marked ready and on the merge queue's group, and pr-title.yaml when the pull request
# is edited or a review on it is submitted or edited); and in its tests, in ci.yaml's test job and
# its private-names-port job. Each of those steps maps PRIVATE_NAME_TERMS.
set -euo pipefail

# A relative path argument is read from the caller's directory, so it is prefixed with that
# directory's path from the repository root (or its absolute path, outside the repository) before
# the cd below. The path is never rewritten: the kernel follows its symlinks and `..` parts as the
# caller's shell would, and a name starting with "-" gets a "./" so find does not read an option.
# Each directory is read with an "x" after pwd's newline, since a command substitution drops every
# trailing newline, a directory name's own included.
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd -P && printf x)
repo_root=${repo_root%$'\n'x}
caller=$(pwd -P && printf x)
caller=${caller%$'\n'x}
case $caller in
  "$repo_root") prefix= ;;
  "$repo_root"/*) prefix=${caller#"$repo_root"/}/ ;;
  *) prefix=$caller/ ;;
esac
args=()
for arg in "$@"; do
  case $arg in
    '' | /*) args+=("$arg") ;;
    -*) args+=("${prefix:-./}$arg") ;;
    *) args+=("$prefix$arg") ;;
  esac
done
set -- "${args[@]}"
cd "$repo_root"

encoded_byte='%[[:xdigit:]]{2}'
dashes='‐|‑|‒|–|—|―|−|﹘|﹣|－'
no_break_space=$'\xc2\xa0'
figure_space=$'\xe2\x80\x87'
narrow_no_break_space=$'\xe2\x80\xaf'
spaces="( |$no_break_space|$figure_space|$narrow_no_break_space)+"
# One separator between the two parts of a name: a hyphen or underscore, a run of spaces or
# no-break spaces, a typographic dash pasted from a document, or a percent-encoded byte.
separator="[-_]|$spaces|$encoded_byte|$dashes"
repository_rule="(^|[^[:alnum:]]|$encoded_byte)agent($separator)?c([^[:alnum:]]|\$)"
company_rule="t""rajectory($separator|[.])?labs"
proper_noun_rule="(^|[^[:alnum:]])(T""rajectory|T""RAJECTORY)([^[:alnum:]]|\$)"
private_host_rule='[[:alnum:]-]+[.]internal[.][[:alnum:].-]+'
# Whole matches of the host rule that name no private host: a reserved example host (a trailing
# full stop is the sentence's), and the NATS client's property in the bundle that vendors it.
allowed_host='[[:alnum:]-]+[.]internal[.]example([.](com|net|org))?[.]*'
vendored_nats='packages/claude-envoy/dist/envoy-channel[.]js'
vendored_property='this[.]internal[.][[:alnum:]_]+'
# A Tailscale MagicDNS host: one DNS label immediately before the literal, with no letter or digit
# right after it (a GNU "\b" word boundary would miss a host that sits next to a "_").
tailnet_host_rule='[[:alnum:]-]+[.]ts[.]net([^[:alnum:]]|$)'
# A whole match of the tailnet rule that names no private tailnet: the reserved example tailnet
# name (the match is only its last label, so a machine name in front of it passes the same way),
# with the one trailing character the rule itself may have captured.
allowed_tailnet_host='example-tailnet[.]ts[.]net[^[:alnum:]]?'

repository_label='names the private deployment repository or its Dispatch project key'
company_label='names the company'
private_host_label='names a private internal host'
tailnet_host_label='names a Tailscale MagicDNS host'
term_label="names a term of the deployment's own list (PRIVATE_NAME_TERMS)"

lockfiles=(bun.lock go.sum go.work.sum)
targets=("$@")

for target in "${targets[@]}"; do
  if [ ! -e "$target" ]; then
    echo "check-private-names: $target does not exist, so there is nothing to scan there" >&2
    exit 2
  fi
done

# The fifth rule's terms: each line of PRIVATE_NAME_TERMS with the blanks around it dropped, and
# none left empty (the header says why).
terms=()
while IFS= read -r term; do
  term=${term#"${term%%[![:space:]]*}"}
  term=${term%"${term##*[![:space:]]}"}
  if [ -n "$term" ]; then terms+=("$term"); fi
done <<< "${PRIVATE_NAME_TERMS:-}"
if [ ${#terms[@]} -eq 0 ]; then
  case ${GITHUB_EVENT_NAME:-} in
    '' | pull_request | pull_request_review)
      echo "::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty, so only the" \
        "rules this script spells ran, and no term of the deployment's own list was checked" >&2
      ;;
    *)
      echo "::error::check-private-names: PRIVATE_NAME_TERMS is unset or empty on a" \
        "$GITHUB_EVENT_NAME run, which must check the deployment's own list: map" \
        "'PRIVATE_NAME_TERMS: \${{ secrets.PRIVATE_NAME_TERMS }}' into this step's env, and" \
        "write that Actions secret if the repository has none" >&2
      exit 2
      ;;
  esac
fi

matches=$(mktemp)
hits=$(mktemp)
paths=$(mktemp)
terms_file=$(mktemp)
trap 'rm -f "$matches" "$hits" "$paths" "$terms_file"' EXIT

if [ ${#targets[@]} -eq 0 ]; then
  git -c core.quotePath=false ls-files --cached --others --exclude-standard | sort -u > "$paths"
  scanned="$(wc -l < "$paths") files in the repository"
else
  find "${targets[@]}" > "$paths"
  files=0
  while IFS= read -r path; do
    if [ -f "$path" ] && [ ! -L "$path" ]; then files=$((files + 1)); fi
  done < "$paths"
  scanned="$files files under ${targets[*]}"
fi

# names <text>: whether one of the first two rules, or a term of the fifth, matches text.
names() {
  local term
  [[ ${1,,} =~ $repository_rule || ${1,,} =~ $company_rule || $1 =~ $proper_noun_rule ]] && return
  for term in "${terms[@]}"; do
    [[ ${1,,} == *"${term,,}"* ]] && return
  done
  return 1
}

# mask <path>: path with each component the first two rules or a term match replaced by <name>,
# then each term still in it, which is one that spans components (an owner/repository form),
# replaced by <name> where it stands, in any case.
mask() {
  local IFS=/ part term masked_path
  local -a parts masked=()
  read -r -a parts <<< "$1"
  for part in "${parts[@]}"; do
    if names "$part"; then masked+=("<name>"); else masked+=("$part"); fi
  done
  masked_path=${masked[*]}
  shopt -s nocasematch
  for term in "${terms[@]}"; do masked_path=${masked_path//"$term"/<name>}; done
  shopt -u nocasematch
  printf '%s' "$masked_path"
}

# run <command...>: appends the command's output to $matches; a failed search (exit 2 or more,
# where grep's 1 means no match) stops the check.
run() {
  local status=0
  "$@" >> "$matches" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "check-private-names: the search with $1 failed (exit $status)" >&2
    exit 2
  fi
}

# search <label> <grep pattern option...>: appends one "path:line: label" row per line where the
# pattern (`-E -e <rule>`, or `-F -f <terms file>`) matches something other than an allowed host.
search() {
  local label=$1 path line
  shift
  : > "$matches"
  if [ ${#targets[@]} -eq 0 ]; then
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=(":(exclude,glob)**/$lockfile"); done
    # --untracked adds the files git has not been told about, but skips a tracked file under an
    # ignored path, which the plain pass reads. core.quotePath=false prints a non-ASCII path as it
    # is, so the annotation names a real file.
    run git -c core.quotePath=false grep -n -o -I --untracked "$@" -- . "${excludes[@]}"
    run git -c core.quotePath=false grep -n -o -I "$@" -- . "${excludes[@]}"
  else
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=("--exclude=$lockfile"); done
    run grep -r -n -o -H -I "${excludes[@]}" "$@" -- "${targets[@]}"
  fi
  { grep -v -i -E -e "^[^:]*:[0-9]+:($allowed_host)\$" \
    -e "^$vendored_nats:[0-9]+:$vendored_property\$" \
    -e "^[^:]*:[0-9]+:($allowed_tailnet_host)\$" "$matches" || true; } |
    cut -d: -f1,2 | while IFS=: read -r path line; do
      printf '%s:%s: %s\n' "$(mask "$path")" "$line" "$label"
    done >> "$hits"
}

# search_paths <label> <grep pattern option...>: appends one "path:0: label in its path" row per
# file or directory whose path the pattern matches.
search_paths() {
  local label=$1 path
  shift
  : > "$matches"
  run grep "$@" "$paths"
  while IFS= read -r path; do
    printf '%s:0: %s in its path\n' "$(mask "$path")" "$label"
  done < "$matches" >> "$hits"
}

search "$repository_label" -E -i -e "$repository_rule"
search "$company_label" -E -i -e "$company_rule"
search "$company_label" -E -e "$proper_noun_rule"
search "$private_host_label" -E -i -e "$private_host_rule"
search "$tailnet_host_label" -E -i -e "$tailnet_host_rule"
search_paths "$repository_label" -E -i -e "$repository_rule"
search_paths "$company_label" -E -i -e "$company_rule"
search_paths "$company_label" -E -e "$proper_noun_rule"
if [ ${#terms[@]} -gt 0 ]; then
  printf '%s\n' "${terms[@]}" > "$terms_file"
  search "$term_label" -F -i -f "$terms_file"
  search_paths "$term_label" -F -i -f "$terms_file"
fi

if [ -s "$hits" ]; then
  sort -t: -k1,1 -k2,2n -u -o "$hits" "$hits"
  while IFS= read -r hit; do
    path=${hit%%:*}
    rest=${hit#*:}
    line=${rest%%:*}
    if [ "$line" = 0 ]; then
      echo "::error::$path: ${rest#*: }"
    else
      echo "::error file=$path,line=$line::$hit"
    fi
  done < "$hits"
  echo "check-private-names: $(wc -l < "$hits") finding(s) ($scanned). This repository is public, so:"
  echo "  - for the deployment repository write \"the deployment repository\" or <deployment repo>, and"
  echo "    use a neutral project key such as ACME in a fixture or example;"
  echo "  - for the company write \"the company\". Its first word capitalized or in capitals is the"
  echo "    company's name wherever it stands, at the start of a sentence or a heading or before a \"_\""
  echo "    too, so reword that sentence or rename that constant;"
  echo "  - for a host write a reserved example host such as <name>.internal.example. Code that only"
  echo "    looks like one, such as a field chain <value>.internal.<field> or a module file named"
  echo "    that way, is refused too: rename it, or reach the field through a variable;"
  echo "  - for a Tailscale MagicDNS host write <tailnet>.ts.net or <name>.<tailnet>.ts.net, or a"
  echo "    host under the reserved example-tailnet.ts.net. Code that only looks like one, such as"
  echo "    an unbraced shell variable glued right before the literal, is refused too: brace the"
  echo "    variable or build the suffix separately;"
  echo "  - for a term of PRIVATE_NAME_TERMS, the deployment's own list, write the neutral form above"
  echo "    that fits it (\"the deployment repository\", \"the company\", a placeholder id). The list"
  echo "    is an Actions secret, so this log never prints the term: read the reported line;"
  echo "  - for a file or directory whose path is reported with <name> in it, rename it."
  exit 1
fi

named="the private deployment repository or the company"
if [ ${#terms[@]} -gt 0 ]; then
  named="the private deployment repository, the company or a term of PRIVATE_NAME_TERMS"
  named+=" (${#terms[@]} terms)"
fi
echo "check-private-names: no file or path names $named, and no text names a private internal" \
  "host or a Tailscale MagicDNS host ($scanned)"
