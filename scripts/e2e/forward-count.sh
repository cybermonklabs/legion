#!/usr/bin/env bash
# The live count of the returns of the retro and the merge, over a window of days and the projects
# a Legion daemon runs (C7 of dispatch://INFRA-5581's design; the checks `c-forward-count-history`
# and `v-forward-count`). It runs the count program's two phases:
#
#   bash scripts/e2e/forward-count.sh --from 2026-11-10 --to 2026-11-23 [--readings <file>]
#   bash scripts/e2e/forward-count.sh --from <release day> --days 14 --readings <readings.jsonl>
#
# `collect` reads Dispatch for each project's returns and their reasons, GitHub for each pull
# request's timeline, and the roles' own transcripts in the transcript store through `tq`. With
# `--readings` it then runs `resolve`, which writes `resolved.jsonl`, `summary.txt` for the window
# and `summary-<PROJECT>.txt` for each project, and prints the window's summary. Without it the
# run stops after `collect`, with `returns.jsonl` for the readers to read.
#
# Options: `--from YYYY-MM-DD` (required), `--to YYYY-MM-DD` or `--days N` (default: `--to` is
# `--from`), `--projects K1,K2,…` (default INFRA,OSSV,BET,LINUX, the projects a Legion daemon
# runs), `--reasons all|none` (default all), `--readings <file>`, `--out <dir>` (default a fresh
# directory under TMPDIR, kept and printed), and `--github-api-url <url>` (default
# https://api.github.com).
#
# It runs where the reader runs `dispatch` by hand, with the reader's own credential: `bun`, `gh`
# signed in, `jq`, and `tq` on PATH for the team's transcript store (its README's "Use" says how
# to install `tq`). Dispatch is the one the reader's shell names: `DISPATCH_URL` and `DISPATCH_TOKEN` or
# `DISPATCH_TOKEN_FILE`, else the `dispatch.serverUrl` and `dispatch.token` of the envoy.json
# `ENVOY_CONFIG_FILE` names (else `$XDG_CONFIG_HOME/omp/envoy.json`). The run writes that bearer
# into a file of its own `mktemp -d`, mode 0600, and deletes that directory at its exit, whatever
# ends the run. It holds no GitHub credential at all: the count reads GitHub through `gh api`,
# which routes each call to the role App of the repository the path names, because a session of
# this fleet may not read a person's own `gh auth token`. It reads and starts nothing else: no
# write of any kind reaches Dispatch or GitHub.
#
# It exits 0 when the run read what it was asked for, 1 when `collect` or `resolve` failed, and 2
# with `UNKNOWN: <reason>` when it could read nothing to judge: no credential, no `tq`, no `bun`,
# or a usage error. An empty or failed read is never a pass.
set -euo pipefail

usage="usage: scripts/e2e/forward-count.sh --from YYYY-MM-DD [--to YYYY-MM-DD | --days N] [--projects K1,K2] [--reasons all|none] [--readings FILE] [--out DIR] [--github-api-url URL]"

unknown() {
  echo "UNKNOWN: $*"
  exit 2
}

root=$(cd "$(dirname "$0")/../.." && pwd)
from=""
to=""
days=""
projects="INFRA,OSSV,BET,LINUX"
reasons="all"
readings=""
out=""
github_api="https://api.github.com"
while [ "$#" -gt 0 ]; do
  case $1 in
    --from) from=${2:-} && shift 2 ;;
    --to) to=${2:-} && shift 2 ;;
    --days) days=${2:-} && shift 2 ;;
    --projects) projects=${2:-} && shift 2 ;;
    --reasons) reasons=${2:-} && shift 2 ;;
    --readings) readings=${2:-} && shift 2 ;;
    --out) out=${2:-} && shift 2 ;;
    --github-api-url) github_api=${2:-} && shift 2 ;;
    -h | --help)
      echo "$usage"
      exit 0
      ;;
    *) unknown "$1 is no option of this script. $usage" ;;
  esac
done

[[ $from =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || unknown "--from takes a day as YYYY-MM-DD. $usage"
if [ -n "$days" ]; then
  [ -z "$to" ] || unknown "--to and --days do not go together. $usage"
  [[ $days =~ ^[0-9]+$ ]] || unknown "--days takes a whole number of days. $usage"
  to=$(date -u -d "$from + $((days - 1)) days" +%Y-%m-%d) || unknown "this machine's date cannot read --from $from"
fi
[ -n "$to" ] || to=$from
[[ $to =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || unknown "--to takes a day as YYYY-MM-DD. $usage"
case $reasons in
  all | none) ;;
  *) unknown "--reasons takes all or none. $usage" ;;
esac
[ -z "$readings" ] || [ -f "$readings" ] || unknown "--readings names $readings, which is no file"

for tool in bun gh jq tq; do
  command -v "$tool" >/dev/null || unknown "no $tool is on PATH, so nothing was read"
done

private=$(mktemp -d "${TMPDIR:-/tmp}/forward-count.XXXXXXXX")
chmod 700 "$private"
trap 'rm -rf "$private"' EXIT

# The reader's own Dispatch credential: the shell's, else the envoy.json the session names. The
# token is written to a file of this run's own, mode 0600, which the trap above deletes.
config=${ENVOY_CONFIG_FILE:-${XDG_CONFIG_HOME:-$HOME/.config}/omp/envoy.json}
url=${DISPATCH_URL:-}
if [ -z "$url" ] && [ -f "$config" ]; then
  url=$(jq -r '.dispatch.serverUrl // empty' "$config")
fi
[ -n "$url" ] || unknown "no Dispatch is named: set DISPATCH_URL, or give $config a dispatch.serverUrl"
token_file="$private/dispatch-token"
(
  umask 077
  if [ -n "${DISPATCH_TOKEN_FILE:-}" ]; then
    cat -- "$DISPATCH_TOKEN_FILE"
  elif [ -n "${DISPATCH_TOKEN:-}" ]; then
    printf '%s\n' "$DISPATCH_TOKEN"
  elif [ -f "$config" ]; then
    jq -r '.dispatch.token // empty' "$config"
  fi
) >"$token_file"
[ -s "$token_file" ] || unknown "no Dispatch credential is named: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or give $config a dispatch.token"

# GitHub is read through `gh api`, which the count runs itself when the run names no token file.
# A reader's own `gh auth token` is refused in a session of this fleet ("no agent process may use
# it"), and `gh` routes each call to the role App of the repository the path names, so this run
# reads GitHub as the fleet reads it and holds no GitHub credential of its own.
# The probe is a read, never `gh auth status`: a session of this fleet has no user login of its
# own, and `gh auth status` reports that failed login while `gh api` still reads GitHub through
# the role App.
gh api rate_limit --jq '.resources.core.limit' >/dev/null 2>&1 ||
  unknown "gh api answered nothing: run gh auth login, or run this where gh reads a role App"

[ -n "$out" ] || out=$(mktemp -d "${TMPDIR:-/tmp}/forward-count-out.XXXXXXXX")
mkdir -p "$out"

program="$root/packages/pi-legion/scripts/forward-count/forward-count.ts"
echo "== collect"
echo "   $projects, $from to $to, reasons $reasons, transcripts tq, Dispatch $url, GitHub through gh api ($github_api)"
bun "$program" collect \
  --dispatch-url "$url" --token-file "$token_file" \
  --github-api-url "$github_api" \
  --projects "$projects" --from "$from" --to "$to" \
  --transcripts tq --reasons "$reasons" --out "$out"
echo "ok collect ($out/returns.jsonl)"

if [ -z "$readings" ]; then
  echo "forward count: collected $(wc -l <"$out/returns.jsonl") returns into $out/returns.jsonl; give --readings <file> to resolve them"
  exit 0
fi

echo "== resolve"
bun "$program" resolve --out "$out" --readings "$readings"
echo "ok resolve ($out/resolved.jsonl, $out/summary.txt)"
echo "forward count: $out"
