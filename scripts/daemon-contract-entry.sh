#!/usr/bin/env bash
# Writes a new daemon API contract entry, the one file a change to the contract adds to
# packages/daemon/internal/api: contract_<id>_<slug>.go, <id> the current UTC time to the second
# (`date -u +%Y%m%d%H%M%S`), and prints its path from the repository root. No change picks a
# contract number: a build numbers its entries by their rank in main's merge order
# (scripts/daemon-contract-order.sh), so two changes add two files and edit no shared line.
#
# The file holds the entry's text, which the author writes in place of the bracketed line, its
# declaration, an untyped constant <lowerCamel(slug)>Contract whose value is the id, by which
# every row the entry adds to a table names it, and its registration. It writes no fixture:
# `go test ./internal/api -update` writes packages/contracts/fixtures/daemon-api/contracts/
# <id>_<slug>.json from the rows.
#
# usage: scripts/daemon-contract-entry.sh <slug>
#   <slug>  the entry's name: [a-z0-9]+(-[a-z0-9]+)*, beginning with a letter, since the
#           constant is the slug in lowerCamel case (fix-attempts: fixAttemptsContract)
#   exit: 0 written, 1 refused (it names why), 2 a usage error
#
# It refuses a slug of another form, and an entry of its slug or of its id that the package
# already holds: two entries of one slug declare one constant, and two of one id are refused by
# TestNoTwoEntriesShareAnId. Run it again a second later for a second entry of one second.
#
# Run from anywhere: scripts/daemon-contract-entry.sh <slug>; it writes into the checkout that
# holds it.
set -euo pipefail

readonly name=daemon-contract-entry.sh
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root
readonly api=packages/daemon/internal/api

usage() {
  sed -n '/^# usage: /,/^#   exit: /p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

[[ $# == 1 ]] || usage
slug=$1
[[ $slug =~ ^[a-z][a-z0-9]*(-[a-z0-9]+)*$ ]] ||
  { echo "${name}: '${slug}' is no slug: it must be [a-z0-9]+(-[a-z0-9]+)* and begin with a letter" >&2; exit 1; }

id=$(date -u +%Y%m%d%H%M%S)
path="${api}/contract_${id}_${slug}.go"

# held <glob> <what>: refuses an entry the glob matches, naming it and what it shares.
held() {
  local file base
  for file in "$root/$api"/$1; do
    base=${file##*/}
    [[ $base =~ ^contract_[0-9]{14}_[a-z0-9]+(-[a-z0-9]+)*[.]go$ ]] || continue
    echo "${name}: ${api}/${base} is an entry of $2 already" >&2
    exit 1
  done
}
shopt -s nullglob
held "contract_[0-9]*_${slug}.go" "the slug ${slug}"
held "contract_${id}_*.go" "the id ${id}"

# lowerCamel(slug): each hyphen dropped and the letter after it raised.
constant=$(printf '%s\n' "$slug" |
  awk -F- '{ s = $1; for (i = 2; i <= NF; i++) s = s toupper(substr($i, 1, 1)) substr($i, 2); print s "Contract" }')

# noclobber: the file is created here, never overwritten, though another run wrote it since the
# check above.
set -o noclobber
printf '%s\n' \
  'package api' \
  '' \
  "// ${id} ${slug}: <the entry's text: what this change does to the contract, and why>." \
  "const ${constant} = ${id}" \
  '' \
  'func init() {' \
  "	registerEntry(contractEntry{id: ${constant}, slug: \"${slug}\"})" \
  '}' >"$root/$path"
echo "$path"
