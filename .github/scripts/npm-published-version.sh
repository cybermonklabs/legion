#!/usr/bin/env bash
# The version npm's registry publishes for one package: `.github/scripts/release-push.sh#version`
# reads it in place of a tag when a release line has no tag yet and --first-npm names the package.
#
# Usage: npm-published-version.sh <package>
#
# One `npm view <package> version --json`, and four outcomes:
#   npm answers       the version, read as the JSON string npm prints on standard output, npm's
#                     own standard error passed through, exit 0.
#   npm answers E404  the registry has no such package, which is a release line's first release:
#                     0.0.0 on standard output, one line on standard error saying npm has no such
#                     package, exit 0.
#   npm fails         a 5xx, a timeout, a refused connection: npm's standard error passed through,
#                     nothing on standard output, and npm's exit status.
#   npm answers no    npm exits 0 but its standard output holds no JSON string (empty, or an
#   version           object; npm 11.19.1 printed nothing for a registry 200 whose body is
#                     {"error":{"code":"E404"}}): npm's standard error passed through,
#                     `npm answered no version of <package>: …` on standard error, nothing on
#                     standard output, exit 1.
# retry_fetch retries the last two, as it retries every failure.
#
# npm's standard error is passed through with each carriage return made a space. npm 11.19.1
# writes a registry error body's text there with a lone carriage return kept (its
# lib/utils/format.js leaves \r out of the control characters it escapes), and it puts
# `npm error ` in front of every line it ends with a newline, so a carriage return is the one way
# a body's text can start a line of its own. GitHub's runner reads a step's output a line at a
# time, ends a line at a lone carriage return as at a newline, and reads a line that starts with
# `::` as a workflow command: npm's line for a 504 whose body's `error` field is "upstream timed
# out\r::warning::x" would give the log a `::warning::x` line, a warning the registry wrote
# (dispatch://INFRA-1545, round 54).
#
# The code is read from the JSON error object npm prints on standard output with --json, its
# `error.code`, and it counts only when npm fails. npm sets it from the registry's HTTP status
# (E404 for a 404, E504 for a 504) or from its own failure (FETCH_ERROR for a timeout): measured
# with npm 11.19.1 against a local registry and the real one (dispatch://INFRA-1545, round 52).
# So no text in a registry's error body decides the code. npm appends a body's `error` field to
# its message, copies that message into the JSON `summary`, and writes each line of it on
# standard error as an `npm error ` line of its own: a 504 whose body holds a newline and then
# `code E404` makes npm write an `npm error code E404` line of its own on standard error, which
# round 51's match on those lines read as no such package. The one other E404 is npm's own
# verdict on a 200's packument, one that marks the package unpublished (`time.unpublished`) or
# whose `dist-tags.latest` names a version it does not hold (npm's lib/commands/view.js): each
# is read here as no such package, as it was before --json.
#
# It is a program, and E404 is a success here, because release-push.sh runs it under retry_fetch
# (.github/scripts/ci-retry.sh): that helper runs its command under `timeout`, which runs a
# program and cannot run a shell function, and it retries every failure. An E404 is the
# registry's answer and not a fault, so it must succeed on the first attempt, with no retry and
# no `::error::`; every other failure is left to the retry.
#
# The `version` cases of release-push.test.sh run it, against a stub npm first on PATH.
set -euo pipefail

package=${1:?usage: npm-published-version.sh <package>}
# npm's standard error is kept, then passed on, each carriage return a space. An interrupt, such
# as retry_fetch's `timeout` at its bound, still runs the trap: bash runs EXIT on SIGTERM.
err=$(mktemp)
trap 'rm -f "$err"' EXIT
status=0
answer=$(npm view "$package" version --json 2> "$err") || status=$?
code=$(jq -r '.error.code?' <<< "$answer" 2> /dev/null) || code=''
if [ "$status" -ne 0 ] && [ "$code" = E404 ]; then
  echo "npm-published-version.sh: npm has no package $package (E404), so its published version is 0.0.0" >&2
  echo 0.0.0
  exit 0
fi
tr '\r' ' ' < "$err" >&2
[ "$status" -eq 0 ] || exit "$status"
version=$(jq -r 'strings' <<< "$answer" 2> /dev/null) || version=''
if [ -z "$version" ]; then
  echo "npm-published-version.sh: npm answered no version of $package: $answer" >&2
  exit 1
fi
printf '%s\n' "$version"
