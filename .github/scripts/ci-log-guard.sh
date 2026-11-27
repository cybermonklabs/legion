#!/bin/sh
# shellcheck shell=sh disable=SC3043 # `local`: dash, busybox ash and bash all have it
# Keeps a server's text in the job log from becoming a workflow command.
#
# The runner reads a step's standard output and standard error line by line, and it ends a line at
# `\n`, at `\r\n` and at a lone `\r` (actions/runner src/Runner.Sdk/ProcessInvoker.cs:513,
# StreamReader.ReadLine). Every line of either stream goes to the command parser
# (src/Runner.Worker/Handlers/ScriptHandler.cs:335-336). A line is a workflow command when it starts
# with `::` after its leading white space is removed, or when it holds `##[<command>]` anywhere
# (src/Runner.Common/ActionCommand.cs:62-63 and :132). So a reply body that go, npm, gh, git, crane
# or curl copies to the log can write `\r::warning::…`, `\t::error::…` or `… ##[warning]…`, and the
# runner acts on it.
#
# GitHub's answer is the pair `::stop-commands::<token>` … `::<token>::`
# ("Stopping and starting workflow commands", GitHub's workflow commands reference): between them
# the runner acts on no line but the one that names the token. The token is random, so the server
# cannot write it.
#
# Every block this file writes goes on standard error, and the text it guards goes there too.
# The runner keeps one stop state for the whole step, not one for each stream
# (src/Runner.Worker/ActionCommandManager.cs:27-28 and :86-124), and it reads the two streams on two
# threads and handles each batch's standard error lines first (ProcessInvoker.cs:417-440). A
# block's markers and its text on one stream stay in order. A block on standard output around
# text on standard error does not (run through the runner's own reader, 30 of 50 runs acted on the
# text), and two blocks on two streams can end each other early. So no caller puts a block on
# standard output.
#
# A step's own workflow commands (`::error::`, `::group::`, `::add-mask::`) go outside a block,
# where the runner still acts on them. Beside a guarded command, write them to standard error,
# where they stay in order with the block: the runner promises no order between the two streams
# (src/Runner.Common/ProcessInvoker.cs:103-109).
#
# Functions:
#
#   ci_log_token                  prints a fresh 32-character hex token from 16 bytes of
#                                 /dev/urandom.
#   ci_log_block_open TOKEN       writes `::stop-commands::TOKEN` on standard error.
#   ci_log_block_close TOKEN      writes `::TOKEN::` on standard error, on a line of its own.
#   ci_log_guard CMD...           runs CMD with its standard output sent to standard error, inside
#                                 a block, and returns CMD's exit status: for output that only goes
#                                 to the log.
#   ci_log_guard_stderr CMD...    runs CMD inside a block around its standard error alone, and
#                                 returns CMD's exit status; CMD's standard output goes where the
#                                 caller sent it, so `v=$(ci_log_guard_stderr gh api …)` reads a
#                                 value with no marker in it.
#
# The two guards run CMD in an `&&`/`||` list, so a caller's `set -e` cannot end the shell before
# the block closes. The shell runs a function passed as CMD without errexit there.
#
# Sourced, never run, and written for POSIX sh with `local` (dash, busybox ash, bash 3.2 and on),
# so a /bin/sh caller can source it as a bash one can. A caller names its path: a bash script beside
# this one as `. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ci-log-guard.sh"`, a sh script by
# a path it already holds (`. "$repo/.github/scripts/ci-log-guard.sh"`; sh has no BASH_SOURCE),
# and a workflow step at the repository root as `. .github/scripts/ci-log-guard.sh`. It sets no
# shell option and leaves no variable behind.
# Tests: .github/scripts/ci-log-guard.test.sh.

ci_log_token() {
  local hex
  hex=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') || return 1
  if [ "${#hex}" -ne 32 ]; then
    echo "ci_log_token: read ${#hex} hex characters from /dev/urandom, not 32" >&2
    return 1
  fi
  printf '%s\n' "$hex"
}

# _ci_log_token_ok TOKEN: true for 32 lowercase hex characters, the form ci_log_token prints. An
# empty token, or one naming a command, makes the runner refuse the stop or never resume.
_ci_log_token_ok() {
  case $1 in
    '' | *[!0-9a-f]*) return 1 ;;
  esac
  [ "${#1}" -eq 32 ]
}

ci_log_block_open() {
  if ! _ci_log_token_ok "${1-}"; then
    echo "ci_log_block_open: '${1-}' is not a token from ci_log_token" >&2
    return 2
  fi
  printf '::stop-commands::%s\n' "$1" >&2
}

# The leading newline ends a last line of the guarded text that has no newline of its own, so the
# close starts a line and the runner reads it; otherwise every line after the block would stay
# stopped.
ci_log_block_close() {
  if ! _ci_log_token_ok "${1-}"; then
    echo "ci_log_block_close: '${1-}' is not a token from ci_log_token" >&2
    return 2
  fi
  printf '\n::%s::\n' "$1" >&2
}

ci_log_guard() {
  local token status
  if [ "$#" -eq 0 ]; then
    echo "ci_log_guard: no command to run" >&2
    return 2
  fi
  token=$(ci_log_token) || return 1
  ci_log_block_open "$token" || return 1
  "$@" >&2 && status=0 || status=$?
  ci_log_block_close "$token" || return 1
  return "$status"
}

ci_log_guard_stderr() {
  local token status
  if [ "$#" -eq 0 ]; then
    echo "ci_log_guard_stderr: no command to run" >&2
    return 2
  fi
  token=$(ci_log_token) || return 1
  ci_log_block_open "$token" || return 1
  "$@" && status=0 || status=$?
  ci_log_block_close "$token" || return 1
  return "$status"
}
