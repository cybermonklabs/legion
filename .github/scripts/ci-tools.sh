#!/usr/bin/env bash
# The one fetch helper for every tool .github/ci-tools.yaml names: it decides the order of the
# sources, verifies the pinned digest, and writes the log line, so no call site repeats any of it
# and no two call sites can disagree about what a refusal means.
#
# A tool this repository installs from a GitHub release asset has a second source, because an hour
# of 504s on GitHub's release assets otherwise holds every merge (dispatch://INFRA-1873). The two
# sources are not equal in rank and the two failures are not alike:
#
#   A transport failure of the primary is a reason to try the second source. Nothing is known
#   about the bytes, because there are none: a 504, a reset, a timeout. That is an outage, and the
#   second source answers it.
#
#   A digest mismatch is never a reason to try another source. The bytes arrived and they are not
#   the pinned ones, which is a supply-chain signal, not an outage. This file stops there, with
#   the source that served those bytes named, and reads nothing else.
#
# Every exit, success or failure, names the source that served the bytes: `github-release`,
# `ghcr-mirror` or `npm`. A failure leaves no file at the destination, so a call site that checks
# the exit status never runs a half-written or unverified file, and one that does not check it
# finds nothing to run.
#
# THIS FILE IS SOURCED, NEVER EXECUTED. It defines functions, declares the empty cache those
# functions keep a read manifest row in, reads nothing and fetches nothing at load, and it is
# not executable (0644). It carries no `set -euo pipefail`, because that would change the shell of
# the caller that sourced it; each function is written to be safe on its own, under a caller that
# has errexit, nounset and pipefail on, as a GitHub `run:` step does, and under one that has none
# of them.
#
#   source "$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh"
#   ci_tools_fetch crane "$RUNNER_TEMP/go-containerregistry_Linux_x86_64.tar.gz"
#
# THE PUBLIC FUNCTIONS.
#
#   ci_tools_field <row> <field>          one field of that row of the manifest, and the
#                                         manifest's own `registry` under that name.
#   ci_tools_digest <row>                 that row's pinned digest.
#   ci_tools_rows [<second source>]       every row's name, in the manifest's own order, and
#                                         only the rows whose second source is that word where
#                                         the argument is given.
#   ci_tools_github_url <url>             that URL with the CI_TOOLS_GITHUB_BASE seam in place
#                                         of its https://github.com prefix.
#   ci_tools_token_of <file>              the `.token` of a registry token response. It is the
#                                         one reader of such an answer this repository has, and
#                                         .github/scripts/mirror-ci-tools.sh reads its own
#                                         pull,push token with it.
#   ci_tools_ghcr_token [<max_time>]      an anonymous pull token for the package `registry:`
#                                         names, which is what a read of a copy carries.
#   ci_tools_verify <row> <file> <source> holds <file> to that digest and writes the line that
#                                         names <source>. It is what an action calls to hold an
#                                         installed binary to its row, so it leaves the file where
#                                         it is, whichever way it answers.
#   ci_tools_fetch <row> <dest>           the primary first, then the second source.
#   ci_tools_fetch_fallback <row> <dest>  the second source alone, for a row whose primary bytes a
#                                         third party fetches: setup-bun fetches Bun's zip and
#                                         mise fetches jj's tarball, so there is no primary here
#                                         to try.
#
# THE FOUR SEAMS, each read with a default, so a CI job sets none of them:
#
#   CI_TOOLS_MANIFEST      the manifest. Default: the ci-tools.yaml beside this file's directory,
#                          resolved from ${BASH_SOURCE[0]}, so a composite action that sources
#                          this file by any path finds the manifest without naming it.
#   CI_TOOLS_GITHUB_BASE   default https://github.com. A row's `primary` and `mirror_from` begin
#                          with that string, and this file replaces exactly that prefix.
#   CI_TOOLS_GHCR_BASE     default https://ghcr.io.
#   CI_TOOLS_NPM_BASE      default https://registry.npmjs.org.
#
# .github/scripts/ci-tools.test.sh serves all three sources from a local HTTP origin through those
# four seams, so every case runs this file's own code and no case reaches the network.

# ci_tools__error <sentence>: a refusal, as a GitHub annotation on stderr.
ci_tools__error() {
  printf '::error::ci-tools: %s\n' "$*" >&2
}

# ci_tools__manifest: the manifest this call reads.
ci_tools__manifest() {
  if [ -n "${CI_TOOLS_MANIFEST:-}" ]; then
    printf '%s\n' "$CI_TOOLS_MANIFEST"
    return 0
  fi
  local here
  here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd) || return 1
  printf '%s\n' "$here/../ci-tools.yaml"
}

# The rows this process has already read, keyed by the manifest's path and the row's name, and
# the fields of the row the last read answered with.
# It is the memory of one process and not a file: a cache on disk would be one more thing that
# can be stale, and that objection stands.
declare -gA ci_tools__row_cache=()
ci_tools__row_fields=''

# ci_tools__row <row>: every field of that row as `<field><TAB><value>` lines in
# ci_tools__row_fields, with the manifest's own `registry` among them, since a ghcr row's second
# source is a blob in that package.
# A successful read is kept for the rest of this process: the file is small, but a full PyYAML
# parse costs about 95 ms and one job makes a hundred of these calls.
# Only a successful read is kept, so a refusal is produced and printed every time it is asked
# for, and the manifest's own path is part of the key, so a caller that moves the
# CI_TOOLS_MANIFEST seam reads the file it named.
# It answers in a variable and not on stdout because a command substitution is a process of its
# own: the row a reader read inside one is a row the shell that asked for it never learns.
ci_tools__row() {
  local row=$1 manifest key
  manifest=$(ci_tools__manifest) || return 1
  key="$manifest"$'\t'"$row"
  if [ -n "${ci_tools__row_cache[$key]+held}" ]; then
    ci_tools__row_fields=${ci_tools__row_cache[$key]}
    return 0
  fi
  ci_tools__row_fields=$(CI_TOOLS_ROW="$row" CI_TOOLS_FILE="$manifest" PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import os
import sys

import yaml

path = os.environ["CI_TOOLS_FILE"]
want = os.environ["CI_TOOLS_ROW"]
try:
    with open(path) as handle:
        document = yaml.safe_load(handle) or {}
except OSError as error:
    sys.stderr.write(f"::error::ci-tools: {want}: this run cannot read {path} ({error})\n")
    raise SystemExit(1)
rows = {row.get("name"): row for row in document.get("tools") or []}
row = rows.get(want)
if row is None:
    held = ", ".join(sorted(name for name in rows if name)) or "no row at all"
    sys.stderr.write(
        f"::error::ci-tools: {want}: {path} holds no row of that name (it holds: {held})\n"
    )
    raise SystemExit(1)
registry = document.get("registry", "")
print(f"registry\t{registry}")
for field, value in row.items():
    print(f"{field}\t{value}")
PY
  ) || return 1
  ci_tools__row_cache[$key]=$ci_tools__row_fields
  return 0
}

# ci_tools__read <variable> <row> <field>: that field of that row into <variable>, which is how
# every reader inside this file asks for one. No command substitution stands between the caller
# and the cache above, so the second field of a row this process already read costs nothing.
ci_tools__read() {
  local __variable=$1 __row=$2 __field=$3 __key __value __held=''
  ci_tools__row "$__row" || return 1
  while IFS=$'\t' read -r __key __value; do
    if [ "$__key" = "$__field" ]; then
      printf -v "$__variable" '%s' "$__value"
      return 0
    fi
    __held="$__held${__held:+, }$__key"
  done <<<"$ci_tools__row_fields"
  ci_tools__error "$__row: this row of the manifest carries no field '$__field' (it carries: $__held)"
  return 1
}

# ci_tools_field <row> <field>: one field of that row, printed. It is what a call site outside
# this file reads a row with.
ci_tools_field() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_field takes a row and a field, and this call gave $#"
    return 1
  fi
  local value
  ci_tools__read value "$1" "$2" || return 1
  printf '%s\n' "$value"
}

# ci_tools_digest <row>: that row's pinned digest.
ci_tools_digest() {
  if [ "$#" -ne 1 ]; then
    ci_tools__error "ci_tools_digest takes a row, and this call gave $#"
    return 1
  fi
  ci_tools_field "$1" digest
}

# ci_tools_rows [<second source>]: every row's name, in the manifest's own order, and only the
# rows whose `second_source` is that word where the argument is given. One reader of the
# manifest's row list, so a job's step names neither the file nor its shape.
ci_tools_rows() {
  if [ "$#" -gt 1 ]; then
    ci_tools__error "ci_tools_rows takes an optional second source, and this call gave $#"
    return 1
  fi
  local manifest names
  manifest=$(ci_tools__manifest) || return 1
  names=$(CI_TOOLS_SECOND="${1:-}" CI_TOOLS_FILE="$manifest" PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import os
import sys

import yaml

path = os.environ["CI_TOOLS_FILE"]
want = os.environ["CI_TOOLS_SECOND"]
try:
    with open(path) as handle:
        document = yaml.safe_load(handle) or {}
except OSError as error:
    sys.stderr.write(f"::error::ci-tools: this run cannot read {path} ({error})\n")
    raise SystemExit(1)
for row in document.get("tools") or []:
    name = row.get("name")
    if not name:
        continue
    if want and row.get("second_source") != want:
        continue
    print(name)
PY
  ) || return 1
  [ -z "$names" ] || printf '%s\n' "$names"
}

# ci_tools_verify <row> <file> <source>: holds <file> to the row's digest, and says so either way.
# It never deletes the file: an action calls it to hold an installed binary to its row, and the
# caller that fetched the file is the one that owns it.
ci_tools_verify() {
  if [ "$#" -ne 3 ]; then
    ci_tools__error "ci_tools_verify takes a row, a file and a source, and this call gave $#"
    return 1
  fi
  local row=$1 file=$2 source=$3 want read_back
  ci_tools__read want "$row" digest || return 1
  read_back=$(sha256sum -- "$file") || {
    ci_tools__error "$row: $source left no file this run can read at $file — refusing"
    return 1
  }
  read_back=${read_back%% *}
  if [ "$read_back" != "$want" ]; then
    ci_tools__error "$row: the bytes from $source are not the pinned ones (pinned sha256 $want, read sha256 $read_back) — refusing"
    return 1
  fi
  printf 'ci-tools: %s: %s served the bytes, sha256 %s\n' "$row" "$source" "$want"
}

# ci_tools__tempfile <row>: a file under the runner's own temporary directory. Every path below
# that does not move it to the destination removes it.
ci_tools__tempfile() {
  local row=$1 directory
  directory=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
  mktemp "$directory/ci-tools-${row//[^A-Za-z0-9._-]/-}.XXXXXX"
}

# ci_tools__curl <file> <max_time> <url> [curl argument…]: the one fetch this file makes.
# `--fail` is what keeps a 504's body out of <file>, `--retry 7 --retry-all-errors` rides out a
# flap of the service, and `--max-time` bounds one attempt, so a hung attempt becomes the exit
# code that sends the caller to the second source.
ci_tools__curl() {
  local file=$1 max_time=$2 url=$3
  shift 3
  curl -fsSL --retry 7 --retry-all-errors --max-time "$max_time" -o "$file" "$@" "$url"
}

# ci_tools_github_url <url>: that URL with the CI_TOOLS_GITHUB_BASE seam in place of its
# https://github.com prefix. .github/scripts/mirror-ci-tools.sh reads a row's `mirror_from`
# through it, so both readers of a row's URL apply that seam the one way.
ci_tools_github_url() {
  local url=$1
  case "$url" in
    https://github.com/*) printf '%s\n' "${CI_TOOLS_GITHUB_BASE:-https://github.com}${url#https://github.com}" ;;
    *) printf '%s\n' "$url" ;;
  esac
}

# ci_tools__place <row> <file> <dest> <source>: the one end of a successful fetch. It verifies the
# digest before the bytes reach <dest>, so a mismatch leaves nothing there, and it removes <file>
# on every path that does not move it.
ci_tools__place() {
  local row=$1 file=$2 dest=$3 source=$4
  if ! ci_tools_verify "$row" "$file" "$source"; then
    rm -f -- "$file"
    return 1
  fi
  if ! mv -f -- "$file" "$dest"; then
    rm -f -- "$file"
    ci_tools__error "$row: $source served the pinned bytes, and this run could not put them at $dest — refusing"
    return 1
  fi
  return 0
}

# ci_tools_token_of <file>: the `.token` of the JSON a registry's token endpoint answered, and
# nothing where that file holds no readable one. One reader of a token response: this file asks
# for an anonymous pull token and .github/scripts/mirror-ci-tools.sh for a pull,push one, and
# both read the answer here.
ci_tools_token_of() {
  PYTHONDONTWRITEBYTECODE=1 python3 -c '
import json
import sys

with open(sys.argv[1]) as handle:
    print(json.load(handle)["token"])
' "$1" 2>/dev/null
}

# ci_tools_ghcr_token [<max_time>]: an anonymous pull token for the package the manifest's
# `registry:` names. The package is public, so this request carries no credential; ghcr.io
# answers a pull token for it and refuses a blob without one.
# Prints the token, and prints what happened and returns non-zero where the request or the read
# of its answer failed, as the fetch functions below do, so a caller holds the sentence rather
# than a half-read token.
ci_tools_ghcr_token() {
  if [ "$#" -gt 1 ]; then
    ci_tools__error "ci_tools_ghcr_token takes an optional per-attempt bound, and this call gave $#"
    return 1
  fi
  local max_time=${1:-60}
  local base=${CI_TOOLS_GHCR_BASE:-https://ghcr.io}
  local registry path token_file token status=0
  ci_tools__registry registry || return 1
  # The registry's path is the manifest's `registry:` with its host removed.
  path=${registry#*/}
  token_file=$(ci_tools__tempfile ghcr-token) || {
    printf 'this run could not make a temporary file for the pull token\n'
    return 1
  }
  ci_tools__curl "$token_file" "$max_time" \
    "$base/token?service=ghcr.io&scope=repository:$path:pull" || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$token_file"
    printf 'the anonymous pull token request failed (curl exit %s)\n' "$status"
    return 1
  fi
  token=$(ci_tools_token_of "$token_file") || token=''
  rm -f -- "$token_file"
  if [ -z "$token" ]; then
    printf 'the token request answered no .token this run could read\n'
    return 1
  fi
  printf '%s\n' "$token"
}

# ci_tools__registry <variable>: the package every ghcr copy lives in, which the manifest names
# once for the whole file, into that variable. Every row carries it, so the first row answers
# for all of them.
ci_tools__registry() {
  local __variable=$1 first
  first=$(ci_tools_rows) || return 1
  first=${first%%$'\n'*}
  if [ -z "$first" ]; then
    ci_tools__error "the manifest holds no row, so this run cannot read the package the copies live in"
    return 1
  fi
  ci_tools__read "$__variable" "$first" registry
}

# ci_tools__fetch_ghcr <row> <max_time> <file>: the row's blob from the copy in the package the
# manifest's `registry:` names, read with the anonymous pull token above. The blob is already the
# bytes the digest names, so nothing is taken out of it.
# Prints what happened, and returns non-zero, where the service did not serve the bytes.
ci_tools__fetch_ghcr() {
  local row=$1 max_time=$2 file=$3
  local base=${CI_TOOLS_GHCR_BASE:-https://ghcr.io}
  local registry path digest token status=0
  ci_tools__read registry "$row" registry || return 1
  ci_tools__read digest "$row" digest || return 1
  # The registry's path is the manifest's `registry:` with its host removed.
  path=${registry#*/}
  # The token helper prints what went wrong in place of a token, and this is the caller that
  # reports it: the sentence belongs in the refusal this row's fetch builds.
  token=$(ci_tools_ghcr_token "$max_time") || {
    printf '%s\n' "$token"
    return 1
  }
  ci_tools__curl "$file" "$max_time" "$base/v2/$path/blobs/sha256:$digest" \
    -H "Authorization: Bearer $token" || status=$?
  if [ "$status" -ne 0 ]; then
    printf 'the blob request failed (curl exit %s)\n' "$status"
    return 1
  fi
  return 0
}

# ci_tools__fetch_npm <row> <max_time> <file>: the row's binary out of the npm package upstream
# publishes it in. npm needs no copy of ours: it is a service every job already reaches, and it
# holds the same binary upstream puts in the GitHub zip.
# Prints what happened, and returns non-zero, where the service did not serve the bytes.
ci_tools__fetch_npm() {
  local row=$1 max_time=$2 file=$3
  local base=${CI_TOOLS_NPM_BASE:-https://registry.npmjs.org}
  local package version member tarball status=0
  ci_tools__read package "$row" npm_package || return 1
  ci_tools__read version "$row" version || return 1
  ci_tools__read member "$row" npm_member || return 1
  tarball=$(ci_tools__tempfile "$row-npm") || {
    printf 'this run could not make a temporary file for the package tarball\n'
    return 1
  }
  # npm serves a scoped package's tarball under the package name without its scope.
  ci_tools__curl "$tarball" "$max_time" "$base/$package/-/${package##*/}-$version.tgz" || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$tarball"
    printf 'the package tarball request failed (curl exit %s)\n' "$status"
    return 1
  fi
  status=0
  tar -xzOf "$tarball" "$member" > "$file" || status=$?
  rm -f -- "$tarball"
  if [ "$status" -ne 0 ]; then
    printf 'the package tarball holds no %s (tar exit %s)\n' "$member" "$status"
    return 1
  fi
  return 0
}

# ci_tools__second_source <row> <dest> <what the primary did>: the second source alone. The third
# argument is empty where there was no primary to try, and the refusal then names one source.
ci_tools__second_source() {
  local row=$1 dest=$2 primary_report=$3
  local second source fetch max_time file report status=0
  ci_tools__read second "$row" second_source || return 1
  ci_tools__read max_time "$row" max_time || return 1
  # One decision: the source a line and a refusal name, and the function that reads it. A third
  # source added to one of the two and forgotten in the other would leave an empty temporary file
  # for the digest check to refuse, which is the wrong refusal for the wrong reason.
  case "$second" in
    ghcr)
      source=ghcr-mirror
      fetch=ci_tools__fetch_ghcr
      ;;
    npm)
      source=npm
      fetch=ci_tools__fetch_npm
      ;;
    *)
      ci_tools__error "$row: the manifest's second_source is '$second', and this file serves ghcr and npm"
      return 1
      ;;
  esac
  file=$(ci_tools__tempfile "$row") || {
    ci_tools__error "$row: this run could not make a temporary file under ${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
    return 1
  }
  report=$("$fetch" "$row" "$max_time" "$file") || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$file"
    [ -n "$report" ] || report="the fetch failed (exit $status)"
    if [ -n "$primary_report" ]; then
      ci_tools__error "$row: no source served the bytes; github-release: $primary_report; $source: $report"
    else
      ci_tools__error "$row: no source served the bytes; $source: $report"
    fi
    return 1
  fi
  ci_tools__place "$row" "$file" "$dest" "$source"
}

# ci_tools_fetch <row> <dest>: the row's bytes at <dest>, from the primary where it serves them
# and from the second source where it does not.
ci_tools_fetch() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_fetch takes a row and a destination, and this call gave $#"
    return 1
  fi
  local row=$1 dest=$2 primary max_time file status=0
  ci_tools__read primary "$row" primary || return 1
  if [ -z "$primary" ]; then
    # A third party fetches this row's primary bytes, so there is nothing here to try first.
    ci_tools__second_source "$row" "$dest" ''
    return
  fi
  ci_tools__read max_time "$row" max_time || return 1
  file=$(ci_tools__tempfile "$row") || {
    ci_tools__error "$row: this run could not make a temporary file under ${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
    return 1
  }
  ci_tools__curl "$file" "$max_time" "$(ci_tools_github_url "$primary")" || status=$?
  if [ "$status" -eq 0 ]; then
    # The bytes arrived. Whatever they are, this call answers for them: a mismatch here is a
    # supply-chain signal, and the second source is never read after one.
    ci_tools__place "$row" "$file" "$dest" github-release
    return
  fi
  rm -f -- "$file"
  printf 'ci-tools: %s: github-release did not serve the bytes (curl exit %s); trying the second source\n' \
    "$row" "$status"
  ci_tools__second_source "$row" "$dest" "curl exit $status"
}

# ci_tools_fetch_fallback <row> <dest>: the second source alone, for the rows whose primary bytes a
# third party fetches. .github/actions/setup-bun and .github/actions/install-jj call it when their
# own install did not land the file.
ci_tools_fetch_fallback() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_fetch_fallback takes a row and a destination, and this call gave $#"
    return 1
  fi
  ci_tools__second_source "$1" "$2" ''
}
