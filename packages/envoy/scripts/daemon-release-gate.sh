#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.sh [--listeners] <commit>
#
# Says whether every production reader of a message name runs a build at or after <commit> of
# cybermonklabs/legion, the reading a pull request quotes before it moves a producer onto the new
# form of a name (dispatch://INFRA-3862, plan C's rollout). A reader on an older image cannot read
# the new form: a Legion daemon built before t-c-5 takes an issue event named in the new form for
# another project's and acknowledges it with no fact, so moving a producer early stops the fleet's
# workflow in silence.
#
# Without --listeners it reads the image that runs each Legion daemon: for each entry of the
# `agent-infra-legion:projects` list of theorem-labs/agent-infra legion/Pulumi.prod.yaml, its
# `daemonImage` when set, else its `image` (legion/README.md's `daemonImage` row). An entry that runs
# no daemon (`replicas: 0`, or `daemon: false`) is printed SKIPPED, by name. An entry at `runtime:
# tmux` runs its daemon on the project's host VM from `host.legionWorker`'s legion, not from its
# `image` (agent-infra legion/host.go, checkHost), and the gate reads no image for it: it is UNKNOWN
# by name, and so is an entry on any runtime but `kubernetes`, the default. With --listeners it
# reads the three listener pins instead: envoy/Pulumi.prod.yaml `listenerImage`,
# envoy-cluster/Pulumi.prod.yaml `envoyImage`, and dispatch/Pulumi.prod.yaml `listenerImage`, which
# that stack's program sets on its listener in place of `envoyImage`, the Dispatch server's
# (dispatch/main.go, pinListenerImage). A dispatch stack that sets no `listenerImage` is UNKNOWN:
# its deployment requires that key, and the listener runs no other image.
#
# Each pin's org.opencontainers.image.source and .revision labels are read with `docker buildx
# imagetools inspect`. A single image is read whole; an OCI image index, which carries no image
# config of its own (its `.Image.Config.Labels` reads `null`), is read one platform at a time, the
# `unknown/unknown` attestation entries left out, and every platform must name the same source and
# revision. A pin built from https://github.com/cybermonklabs/legion, or from a name the deployment
# records as the same repository (the repositoryRenames pairs of legion/Pulumi.prod.yaml's project
# entries, which the daemon reads as `repository_renames`: sjawhar/legion is this repository under
# its name before the transfer), is compared with `gh api
# repos/cybermonklabs/legion/compare/<commit>...<revision>`, and passes when GitHub answers `ahead`
# or `identical` with `behind_by` 0; its line names the rename it was read by. A pin built from any
# other repository is refused by name and never compared, since its revision names another
# repository's commit, and one built under another name while that record cannot be read is
# UNKNOWN. A revision label that is not a commit id, 7 to 40 lowercase hex digits, is UNKNOWN and
# never compared: GitHub's compare resolves any ref, so a label naming a branch (`main`) would
# compare as wherever that branch points now.
#
# It prints one line per pin: `PASS`, `GATE SHUT` (behind <commit>, or built elsewhere), `UNKNOWN`
# (a label, a stack file or a comparison it could not read, a revision that is no commit id, or a
# daemon whose image it does not read) or `SKIPPED`; then the verdict, alone on
# the last line: `GATE OPEN` (exit 0) only when every pin it read passed and it read at least one,
# `GATE SHUT` (exit 1) when any pin shut it, and `UNKNOWN` (exit 3) otherwise. A usage error exits 2.
#
# It needs bash, gh (signed in with read access to theorem-labs/agent-infra and cybermonklabs/legion),
# docker with buildx, jq, base64 and python3 with PyYAML. Docker must be able to read each pin's
# registry: for ghcr.io, a login with a token holding read:packages, made into a config directory of
# your own so the shared one is untouched:
#   export DOCKER_CONFIG=$(mktemp -d)
#   gh auth token | docker login ghcr.io -u x --password-stdin
# A pin docker cannot read prints UNKNOWN with docker's own answer, never GATE OPEN.
set -euo pipefail

usage() {
  echo "usage: daemon-release-gate.sh [--listeners] <commit>" >&2
  echo "  <commit> is the cybermonklabs/legion commit, 7 to 40 hex digits, every reader must run at or after" >&2
  exit 2
}

mode=daemons
if [ "${1:-}" = --listeners ]; then
  mode=listeners
  shift
fi
[ "$#" -eq 1 ] && [[ $1 =~ ^[0-9a-f]{7,40}$ ]] || usage
readonly commit=$1
readonly legion_source="https://github.com/cybermonklabs/legion"

work=$(mktemp -d)
readonly work
trap 'rm -rf "$work"' EXIT

shut=0
unknown=0
compared=0

first_line() { head -n 1 "$1"; }

# stack_file <path>: the text of <path> in theorem-labs/agent-infra on its default branch, into
# $work/stack.yaml. An unreadable file is UNKNOWN, named with GitHub's answer.
stack_file() {
  local encoded
  if ! encoded=$(gh api "repos/theorem-labs/agent-infra/contents/$1" --jq .content 2>"$work/err"); then
    printf 'UNKNOWN: cannot read theorem-labs/agent-infra %s: %s\n' "$1" "$(first_line "$work/err")"
    return 1
  fi
  if ! printf '%s' "$encoded" | base64 -d >"$work/stack.yaml" 2>"$work/err"; then
    printf 'UNKNOWN: cannot decode theorem-labs/agent-infra %s: %s\n' "$1" "$(first_line "$work/err")"
    return 1
  fi
}

# daemon_pins: one line per entry of $work/stack.yaml's projects list, tab-separated: the entry's
# key, then READ and the image that runs its daemon, SKIP and why it runs none, HOST for an entry
# whose daemon runs on a tmux host, RUNTIME and a runtime the gate does not know, or NOIMAGE. A file
# with no such list prints ERROR and why.
daemon_pins() {
  python3 - "$work/stack.yaml" <<'PY'
import sys

import yaml

with open(sys.argv[1]) as handle:
    document = yaml.safe_load(handle) or {}
projects = (document.get("config") or {}).get("agent-infra-legion:projects")
if not isinstance(projects, list):
    print("ERROR\tit holds no agent-infra-legion:projects list")
    sys.exit(0)
for entry in projects:
    key = entry.get("key") or "<an entry with no key>"
    if entry.get("replicas") == 0:
        print(f"{key}\tSKIP\treplicas: 0")
    elif entry.get("daemon") is False:
        print(f"{key}\tSKIP\tdaemon: false")
    elif entry.get("runtime") == "tmux":
        print(f"{key}\tHOST")
    elif entry.get("runtime") not in (None, "", "kubernetes"):
        print(f"{key}\tRUNTIME\t{entry.get('runtime')}")
    elif entry.get("daemonImage") or entry.get("image"):
        print(f"{key}\tREAD\t{entry.get('daemonImage') or entry.get('image')}")
    else:
        print(f"{key}\tNOIMAGE")
PY
}

# config_value <key>...: the first of the config keys $work/stack.yaml sets, or nothing.
config_value() {
  python3 - "$work/stack.yaml" "$@" <<'PY'
import sys

import yaml

with open(sys.argv[1]) as handle:
    config = (yaml.safe_load(handle) or {}).get("config") or {}
for key in sys.argv[2:]:
    if config.get(key):
        print(config[key])
        break
PY
}

# inspect <ref> <format>: docker's answer for ref in format, into stdout; its error into $work/err.
inspect() {
  docker buildx imagetools inspect "$1" --format "$2" 2>"$work/err"
}

# labels_of <labels json>: sets $source and $revision from the labels, each "" when its label is
# missing (a `null` set of labels holds neither); fails on an answer that is not JSON.
labels_of() {
  source=$(jq -r '.["org.opencontainers.image.source"] // ""' <<<"$1" 2>"$work/err") &&
    revision=$(jq -r '.["org.opencontainers.image.revision"] // ""' <<<"$1" 2>"$work/err")
}

# read_pin <key> <ref>: sets $source and $revision from ref's labels and returns 0, or prints
# UNKNOWN naming what it could not read and returns 1.
read_pin() {
  local key=$1 ref=$2 mediatype labels platforms platform answers="" first="" disagree=0
  source=""
  revision=""
  if ! mediatype=$(inspect "$ref" '{{json .Manifest.MediaType}}'); then
    printf 'UNKNOWN: %s: cannot read %s: %s\n' "$key" "$ref" "$(first_line "$work/err")"
    return 1
  fi
  case "$mediatype" in
    *image.index*|*manifest.list*)
      if ! platforms=$(inspect "$ref" '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}}{{if .Platform.Variant}}/{{.Platform.Variant}}{{end}} {{end}}'); then
        printf 'UNKNOWN: %s: cannot list the platforms of %s: %s\n' "$key" "$ref" "$(first_line "$work/err")"
        return 1
      fi
      for platform in $platforms; do
        [ "$platform" != unknown/unknown ] || continue
        if ! labels=$(inspect "$ref" "{{json (index .Image \"$platform\").Config.Labels}}"); then
          printf 'UNKNOWN: %s: cannot read the %s image of %s: %s\n' "$key" "$platform" "$ref" "$(first_line "$work/err")"
          return 1
        fi
        if ! labels_of "$labels"; then
          printf 'UNKNOWN: %s: the %s labels of %s are not JSON: %s\n' "$key" "$platform" "$ref" "$(first_line "$work/err")"
          return 1
        fi
        if [ -z "$first" ]; then
          first="$source $revision"
        elif [ "$first" != "$source $revision" ]; then
          disagree=1
        fi
        answers+="${answers:+, }$platform names ${source:-no source} at ${revision:-no revision}"
      done
      if [ -z "$first" ]; then
        printf 'UNKNOWN: %s: %s is an index with no platform but unknown/unknown\n' "$key" "$ref"
        return 1
      fi
      if [ "$disagree" = 1 ]; then
        printf 'UNKNOWN: %s platforms disagree: %s\n' "$key" "$answers"
        return 1
      fi
      ;;
    *image.manifest*|*distribution.manifest.v2*)
      if ! labels=$(inspect "$ref" '{{json .Image.Config.Labels}}'); then
        printf 'UNKNOWN: %s: cannot read the labels of %s: %s\n' "$key" "$ref" "$(first_line "$work/err")"
        return 1
      fi
      if ! labels_of "$labels"; then
        printf 'UNKNOWN: %s: the labels of %s are not JSON: %s\n' "$key" "$ref" "$(first_line "$work/err")"
        return 1
      fi
      ;;
    *)
      printf 'UNKNOWN: %s: %s has media type %s, which is neither an image nor an index\n' "$key" "$ref" "$mediatype"
      return 1
      ;;
  esac
  if [ -z "$source" ]; then
    printf 'UNKNOWN: %s: %s names no org.opencontainers.image.source label\n' "$key" "$ref"
    return 1
  fi
  if [ -z "$revision" ]; then
    printf 'UNKNOWN: %s: %s names no org.opencontainers.image.revision label\n' "$key" "$ref"
    return 1
  fi
}

# aliases_state is unread, read or failed; load_aliases reads the record once, on the first pin
# built anywhere but legion_source, into $work/aliases.
aliases_state=unread
aliases_error=""

# load_aliases writes into $work/aliases, lowercased and one per line, every name a recorded
# rename makes one repository with cybermonklabs/legion. The record is the deployment's own:
# the repositoryRenames pairs of the project entries of theorem-labs/agent-infra
# legion/Pulumi.prod.yaml, which render as each daemon's `repository_renames` and which the
# daemon's ghrepo.Renames reads (packages/daemon/internal/ghrepo/renames.go). It is the one place
# the transfer of sjawhar/legion to cybermonklabs/legion is recorded, so the gate keeps no copy of
# the pair. Without the list mode has read that file already ($work/legion.yaml); --listeners
# reads it here. A record it cannot read leaves aliases_state failed, with GitHub's or the
# parser's first line in aliases_error.
load_aliases() {
  [ "$aliases_state" = unread ] || return 0
  local encoded
  if [ ! -s "$work/legion.yaml" ]; then
    if ! encoded=$(gh api repos/theorem-labs/agent-infra/contents/legion/Pulumi.prod.yaml --jq .content 2>"$work/err") ||
      ! printf '%s' "$encoded" | base64 -d >"$work/legion.yaml" 2>"$work/err"; then
      aliases_state=failed
      aliases_error=$(first_line "$work/err")
      return 0
    fi
  fi
  if ! python3 - "$work/legion.yaml" >"$work/aliases" 2>"$work/err" <<'PY'; then
import sys

import yaml

with open(sys.argv[1]) as handle:
    document = yaml.safe_load(handle) or {}
projects = (document.get("config") or {}).get("agent-infra-legion:projects") or []
home = "cybermonklabs/legion"
for entry in projects:
    for pair in entry.get("repositoryRenames") or []:
        names = {str(pair.get("from", "")).lower(), str(pair.get("to", "")).lower()}
        if home in names:
            for name in names - {home, ""}:
                print(name)
PY
    aliases_state=failed
    aliases_error=$(first_line "$work/err")
    return 0
  fi
  aliases_state="read"
}

# judge <key> <ref>: reads the pin, holds it to the source rule, compares it, and prints its line.
# A pin built under a name a recorded rename makes one repository with cybermonklabs/legion is a
# build of this repository, compared like any other; its line names the rename it was read by. A
# revision that is not 7 to 40 lowercase hex digits is UNKNOWN and never compared: GitHub's compare
# resolves any ref, so a label naming a branch or a tag (`main`) would compare as wherever that ref
# points now, not as the commit the image was built from.
judge() {
  local key=$1 ref=$2 answer status behind_by repo via=""
  compared=$((compared + 1))
  if ! read_pin "$key" "$ref"; then
    unknown=$((unknown + 1))
    return
  fi
  repo=${source#https://github.com/}
  repo=$(printf '%s' "${repo%.git}" | tr '[:upper:]' '[:lower:]')
  if [ "$repo" != cybermonklabs/legion ]; then
    load_aliases
    if [ "$aliases_state" = failed ]; then
      printf 'UNKNOWN: %s: built from %s, and the repository renames of theorem-labs/agent-infra legion/Pulumi.prod.yaml could not be read: %s\n' "$key" "$source" "$aliases_error"
      unknown=$((unknown + 1))
      return
    fi
    if ! grep -qxF -- "$repo" "$work/aliases"; then
      printf 'GATE SHUT: %s runs an image built from %s\n' "$key" "$source"
      shut=$((shut + 1))
      return
    fi
    via="; built as $source, which legion/Pulumi.prod.yaml's repositoryRenames makes one repository with $legion_source"
  fi
  if ! [[ $revision =~ ^[0-9a-f]{7,40}$ ]]; then
    printf 'UNKNOWN: %s: %s names revision %s, which is no commit id\n' "$key" "$ref" "$revision"
    unknown=$((unknown + 1))
    return
  fi
  if ! answer=$(gh api "repos/cybermonklabs/legion/compare/${commit}...${revision}" --jq '{status,behind_by}' 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot compare %s...%s in cybermonklabs/legion: %s\n' "$key" "$commit" "$revision" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! IFS=$'\t' read -r status behind_by < <(jq -r '[.status, .behind_by] | @tsv' <<<"$answer" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read GitHub'\''s comparison of %s...%s: %s\n' "$key" "$commit" "$revision" "$answer"
    unknown=$((unknown + 1))
    return
  fi
  case "$status:$behind_by" in
    ahead:0) printf 'PASS: %s runs %s (ahead of %s)%s\n' "$key" "$revision" "$commit" "$via" ;;
    identical:0) printf 'PASS: %s runs %s (identical to %s)%s\n' "$key" "$revision" "$commit" "$via" ;;
    behind:*)
      printf 'GATE SHUT: %s runs %s (behind %s by %s)%s\n' "$key" "$revision" "$commit" "$behind_by" "$via"
      shut=$((shut + 1))
      ;;
    diverged:*)
      printf 'GATE SHUT: %s runs %s (diverged from %s, behind by %s)%s\n' "$key" "$revision" "$commit" "$behind_by" "$via"
      shut=$((shut + 1))
      ;;
    *)
      printf 'UNKNOWN: %s: GitHub compared %s...%s as %s, behind by %s\n' "$key" "$commit" "$revision" "$status" "$behind_by"
      unknown=$((unknown + 1))
      ;;
  esac
}

# listener <key> <stack> <config key>...: judges the listener pin of <stack> (envoy, envoy-cluster or
# dispatch), the first of the config keys its Pulumi.prod.yaml sets.
listener() {
  local key=$1 stack=$2 ref
  shift 2
  if ! stack_file "$stack/Pulumi.prod.yaml"; then
    unknown=$((unknown + 1))
    return
  fi
  if ! ref=$(config_value "$@" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read %s/Pulumi.prod.yaml: %s\n' "$key" "$stack" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ -z "$ref" ]; then
    printf 'UNKNOWN: %s: %s/Pulumi.prod.yaml sets no %s\n' "$key" "$stack" "$*"
    unknown=$((unknown + 1))
    return
  fi
  judge "$key" "$ref"
}

if [ "$mode" = listeners ]; then
  listener envoy envoy agent-infra-envoy:listenerImage
  listener envoy-cluster envoy-cluster agent-infra-envoy-cluster:envoyImage
  # dispatch's listener runs listenerImage and nothing else: agent-infra dispatch/main.go requires
  # that key (loadSettings) and pinListenerImage sets the listener's container from it, leaving
  # envoyImage to the Dispatch server. A stack that sets none runs no listener this gate can read,
  # so it is UNKNOWN rather than a reading of the server's image.
  listener dispatch dispatch agent-infra-dispatch:listenerImage
elif stack_file legion/Pulumi.prod.yaml; then
  # The same file records the repository renames (load_aliases), so it is read once.
  cp "$work/stack.yaml" "$work/legion.yaml"
  if ! daemon_pins >"$work/pins" 2>"$work/err"; then
    printf 'UNKNOWN: cannot read theorem-labs/agent-infra legion/Pulumi.prod.yaml: %s\n' "$(first_line "$work/err")"
    unknown=$((unknown + 1))
  fi
  while IFS=$'\t' read -r key what detail; do
    case "$what" in
      SKIP) printf 'SKIPPED: %s runs no daemon (%s)\n' "$key" "$detail" ;;
      READ) judge "$key" "$detail" ;;
      NOIMAGE)
        printf 'UNKNOWN: %s runs a daemon and names neither daemonImage nor image\n' "$key"
        unknown=$((unknown + 1))
        ;;
      HOST)
        printf 'UNKNOWN: %s runs its daemon on a tmux host (runtime: tmux), from host.legionWorker, an image this gate does not read\n' "$key"
        unknown=$((unknown + 1))
        ;;
      RUNTIME)
        printf 'UNKNOWN: %s names runtime %s, which this gate does not know, so it reads no image for its daemon\n' "$key" "$detail"
        unknown=$((unknown + 1))
        ;;
      *)
        printf 'UNKNOWN: theorem-labs/agent-infra legion/Pulumi.prod.yaml: %s\n' "$what"
        unknown=$((unknown + 1))
        ;;
    esac
  done <"$work/pins"
  if [ "$compared" -eq 0 ] && [ "$unknown" -eq 0 ]; then
    echo "UNKNOWN: legion/Pulumi.prod.yaml lists no running daemon, so no pin was read"
    unknown=1
  fi
else
  unknown=$((unknown + 1))
fi

if [ "$shut" -gt 0 ]; then
  echo "GATE SHUT"
  exit 1
fi
if [ "$unknown" -gt 0 ]; then
  echo "UNKNOWN"
  exit 3
fi
echo "GATE OPEN"
