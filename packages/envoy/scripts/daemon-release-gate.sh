#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.sh [--listeners | --clients] <commit>
#
# Says whether every production reader of a message name runs a build at or after <commit> of
# cybermonklabs/legion, the reading a pull request quotes before it moves a producer onto the new
# form of a name (dispatch://INFRA-3862, plan C's rollout). A reader on an older image cannot read
# the new form: a Legion daemon built before t-c-5 takes an issue event named in the new form for
# another project's and acknowledges it with no fact, so moving a producer early stops the fleet's
# workflow in silence.
#
# Every file of the deployment repository it reads, it reads at one commit: the head of that
# repository's default branch when the run starts, which its first line names (`READ: <repository>
# at <sha>`).
#
# Without a mode it reads the image that runs each Legion daemon: for each project file of the
# deployment repository's legion/projects/ (one `<nn>-<KEY>.yaml` per project, read in file-name
# order, as the deployment's program reads them), its `daemonImage` when set, else its `image`
# (legion/README.md's `daemonImage` row). A listed entry that is no such file, a file it cannot
# read, and a file whose `key` is not the KEY of its name are UNKNOWN. An entry that runs no daemon
# (`replicas: 0`, or `daemon: false`) is printed SKIPPED, by name. An entry at `runtime: tmux` runs
# its daemon on the project's host VM from `host.legionWorker`'s legion, not from its `image` (the
# deployment's legion/host.go, checkHost), and the gate reads no image for it: it is UNKNOWN by
# name, and so is an entry on any runtime but `kubernetes`, the default.
#
# With --listeners it reads the listener pins instead: envoy-cluster/Pulumi.prod.yaml
# `envoyImage`, dispatch/Pulumi.prod.yaml `listenerImage`, which that stack's program sets on its
# listener in place of `envoyImage`, the Dispatch server's (the deployment's dispatch/main.go,
# pinListenerImage), and, when the deployment declares its VM hub, envoy/Pulumi.prod.yaml
# `listenerImage`, which only that hub's listener reads. Whether it declares the VM hub is decided
# from the envoy/ program itself, never from the pin: the gate lists envoy/ and reads each `.go`
# file it lists that is not a `_test.go`; a file with a line that calls `NewNatsHub(` (a line that
# is a `//` comment does not count) declares it. Declared, the pin is read, and a stack that sets
# none is UNKNOWN. Declared by no file, with the listing and every read answered, the gate prints
# `NO VM HUB: envoy/ at <sha> declares no gcp:NatsHub` and reads no VM pin. A listing or a read that
# fails, or a listing with no Go file, is UNKNOWN, so a missing pin never reads as a hub that is
# gone. A dispatch stack that sets no `listenerImage` is UNKNOWN: its deployment requires that key,
# and the listener runs no other image.
#
# With --clients it reads the clients that deliver to a session, each of which must carry
# <commit>'s client follow filter, since an older one passes a frame in a form it does not match
# in silence:
#   - in each worker image, each `daemonImage` and `image` of the project files the daemon leg
#     reads (the same entries are SKIPPED or UNKNOWN), the pi-envoy root a pod loads,
#     /opt/legion/pi-envoy/package.json, and the claude-envoy root a Claude worker loads,
#     /opt/legion/claude-envoy/package.json, each read with `docker run --rm --entrypoint cat`;
#     pi-envoy's version is resolved to its release tag, `pi-envoy-v<version>`. The image builds
#     claude-envoy from the checkout its `legion` binary is built from and never gives it a release
#     version (its manifest keeps its source version), so its commit is the one
#     `/opt/legion/bin/legion version` in the same image names. An image with no claude-envoy
#     manifest is UNKNOWN.
#   - on this developer box, the `@sjawhar/pi-envoy` pin of ~/.dotfiles/omp/plugins/package.json,
#     resolved to its release tag, and the claude-envoy version the marketplace `legion-plugins`
#     serves to ~/.dotfiles/.claude/settings.json: the marketplace's
#     `.claude-plugin/marketplace.json`, read from the GitHub repository settings.json names for
#     it, gives an npm package and a version range, the highest version npm matches to the range
#     is the served one, and `claude-envoy-v<version>` its release tag. A settings.json that
#     enables no `claude-envoy@legion-plugins` is SKIPPED. A pi-envoy installed in an Oh My Pi
#     plugin tree that is not ~/.dotfiles/omp/plugins (~/.omp/plugins, or a profile's), and a
#     claude-envoy installed in ~/.claude/plugins, are UNKNOWN: the dotfiles' pins say nothing about
#     them. A session started before a pin moved still runs the plugin it loaded; the gate reads
#     pins, not processes.
#   Each commit is compared with <commit> as a daemon's revision is.
#
# Each pin's org.opencontainers.image.source and .revision labels are read with `docker buildx
# imagetools inspect`. A single image is read whole; an OCI image index, which carries no image
# config of its own (its `.Image.Config.Labels` reads `null`), is read one platform at a time, the
# `unknown/unknown` attestation entries left out, and every platform must name the same source and
# revision. A pin built from https://github.com/cybermonklabs/legion, or from a name the deployment
# records as the same repository (the repositoryRenames pairs of legion/projects/'s project files,
# which the daemon reads as `repository_renames`: sjawhar/legion is this repository under its name
# before the transfer), is compared with `gh api
# repos/cybermonklabs/legion/compare/<commit>...<revision>`, and passes when GitHub answers `ahead`
# or `identical` with `behind_by` 0; its line names the rename it was read by. A pin built from any
# other repository is refused by name and never compared, since its revision names another
# repository's commit, and one built under another name while that record cannot be read is
# UNKNOWN. A revision label that is not a commit id, 7 to 40 lowercase hex digits, is UNKNOWN and
# never compared: GitHub's compare resolves any ref, so a label naming a branch (`main`) would
# compare as wherever that branch points now.
#
# This repository names no deployment, so environment variables name it, each with no default:
#   DAEMON_RELEASE_GATE_REPOSITORY             the deployment repository, owner/name, whose
#                                              legion/projects/ and <stack>/Pulumi.prod.yaml files
#                                              hold the pins; every mode needs it
#   DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX  the prefix of that repository's Pulumi project
#                                              names, which namespace each stack's config keys:
#                                              <prefix>-envoy:listenerImage,
#                                              <prefix>-envoy-cluster:envoyImage and
#                                              <prefix>-dispatch:listenerImage; --listeners needs it
# A run without one it needs, or with it empty, is a usage error, naming it, before anything is read.
#
# It prints the commit it read the deployment at, then one line per pin or surface: `PASS`,
# `GATE SHUT` (behind <commit>, or built elsewhere), `UNKNOWN` (a label, a file, a version or a
# comparison it could not read, a revision that is no commit id, or a daemon whose image it does
# not read), `SKIPPED` or `NO VM HUB`; then the verdict, alone on the last line: `GATE OPEN` (exit
# 0) only when every pin and surface it read passed and it read at least one, `GATE SHUT` (exit 1)
# when any shut it, and `UNKNOWN` (exit 3) otherwise. A usage error, a missing deployment setting
# among them, exits 2.
#
# It needs bash, gh (signed in with read access to the deployment repository and
# cybermonklabs/legion), docker with buildx, jq, python3 with PyYAML and, for --clients, npm.
# Docker must be able to read each pin's registry: for ghcr.io, a login with a token holding
# read:packages, made into a config directory of your own so the shared one is untouched:
#   export DOCKER_CONFIG=$(mktemp -d)
#   gh auth token | docker login ghcr.io -u x --password-stdin
# A pin docker cannot read prints UNKNOWN with docker's own answer, never GATE OPEN. --clients
# pulls each worker image it does not already hold.
set -euo pipefail

usage() {
  echo "usage: daemon-release-gate.sh [--listeners | --clients] <commit>" >&2
  echo "  <commit> is the cybermonklabs/legion commit, 7 to 40 hex digits, every reader must run at or after" >&2
  echo "  DAEMON_RELEASE_GATE_REPOSITORY (owner/name) names the deployment; --listeners also needs DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX" >&2
  exit 2
}

mode=daemons
case "${1:-}" in
  --listeners) mode=listeners && shift ;;
  --clients) mode=clients && shift ;;
esac
[ "$#" -eq 1 ] && [[ $1 =~ ^[0-9a-f]{7,40}$ ]] || usage
readonly commit=$1
readonly legion_source="https://github.com/cybermonklabs/legion"

# require_setting <variable> <what it holds>: a usage error naming <variable> when it is unset or
# empty. Each names the deployment, and this repository holds no default for either.
require_setting() {
  if [ -z "${!1:-}" ]; then
    echo "daemon-release-gate.sh: $1 is unset or empty: it is $2" >&2
    usage
  fi
}
require_setting DAEMON_RELEASE_GATE_REPOSITORY \
  "the deployment repository, owner/name, whose legion/projects/ and <stack>/Pulumi.prod.yaml files hold the pins"
readonly deployment=$DAEMON_RELEASE_GATE_REPOSITORY
if [ "$mode" = listeners ]; then
  require_setting DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX \
    "the prefix of the deployment's Pulumi project names, as in <prefix>-envoy:listenerImage"
  readonly project_prefix=$DAEMON_RELEASE_GATE_PULUMI_PROJECT_PREFIX
fi

work=$(mktemp -d)
readonly work
trap 'rm -rf "$work"' EXIT
mkdir "$work/compared" "$work/tags" "$work/runs" "$work/projects"

shut=0
unknown=0

first_line() { head -n 1 "$1"; }
last_line() { tail -n 1 "$1"; }

# verdict prints the last line and exits with its status.
verdict() {
  if [ "$shut" -gt 0 ]; then
    echo "GATE SHUT"
    exit 1
  fi
  if [ "$unknown" -gt 0 ]; then
    echo "UNKNOWN"
    exit 3
  fi
  echo "GATE OPEN"
  exit 0
}

# The one commit of the deployment repository every read below is made at.
if ! head=$(gh api "repos/$deployment/commits/HEAD" --jq .sha 2>"$work/err"); then
  printf 'UNKNOWN: cannot read the head of %s: %s\n' "$deployment" "$(first_line "$work/err")"
  unknown=1
  verdict
fi
if ! [[ $head =~ ^[0-9a-f]{40}$ ]]; then
  printf 'UNKNOWN: GitHub answered %s for the head of %s, which is no commit id\n' "$head" "$deployment"
  unknown=1
  verdict
fi
readonly head
printf 'READ: %s at %s\n' "$deployment" "$head"

# deployment_file <path> <out>: the bytes of <path> in the deployment repository at $head, into
# <out>, read raw so a large file is never a truncated one; GitHub's error into $work/err.
deployment_file() {
  gh api -H "Accept: application/vnd.github.raw+json" "repos/$deployment/contents/$1?ref=$head" >"$2" 2>"$work/err"
}

# deployment_dir <path> <out>: one line per entry of the directory <path> at $head, its name and
# its type (file, dir, ...) tab-separated, into <out>; GitHub's error into $work/err.
deployment_dir() {
  gh api "repos/$deployment/contents/$1?ref=$head" --jq '.[] | [.name, .type] | @tsv' >"$2" 2>"$work/err"
}

# stack_file <path>: <path> into $work/stack.yaml. An unreadable file is UNKNOWN, named with
# GitHub's answer.
stack_file() {
  if ! deployment_file "$1" "$work/stack.yaml"; then
    printf 'UNKNOWN: cannot read %s %s at %s: %s\n' "$deployment" "$1" "$head" "$(first_line "$work/err")"
    return 1
  fi
}

# projects_state is unread, read or failed; load_projects reads legion/projects/ once into
# $work/projects.json, one JSON list of the project files' mappings in file-name order, and on a
# failure leaves the reason in projects_error.
projects_state=unread
projects_error=""
load_projects() {
  [ "$projects_state" = unread ] || {
    [ "$projects_state" = read ]
    return
  }
  projects_state=failed
  local name type listed=0
  if ! deployment_dir legion/projects "$work/projects.ls"; then
    projects_error="cannot list $deployment legion/projects/ at $head: $(first_line "$work/err")"
    return 1
  fi
  while IFS=$'\t' read -r name type; do
    listed=$((listed + 1))
    if [ "$type" != file ] || ! [[ $name =~ ^[0-9]{2}-[A-Z][A-Z0-9]*\.yaml$ ]]; then
      projects_error="$deployment legion/projects/ at $head holds $name ($type), which is no <nn>-<KEY>.yaml project file"
      return 1
    fi
    if ! deployment_file "legion/projects/$name" "$work/projects/$name"; then
      projects_error="cannot read $deployment legion/projects/$name at $head: $(first_line "$work/err")"
      return 1
    fi
  done <"$work/projects.ls"
  if [ "$listed" -eq 0 ]; then
    projects_error="$deployment legion/projects/ at $head holds no project file"
    return 1
  fi
  if ! python3 - "$work/projects" >"$work/projects.json" 2>"$work/err" <<'PY'; then
import json
import os
import sys

import yaml

directory = sys.argv[1]
projects = []
for name in sorted(os.listdir(directory)):
    with open(os.path.join(directory, name)) as handle:
        entry = yaml.safe_load(handle)
    key = name[3:-len(".yaml")]
    if not isinstance(entry, dict):
        sys.exit(f"legion/projects/{name} is not one mapping of a project's settings")
    if entry.get("key") != key:
        sys.exit(f"legion/projects/{name} declares key {entry.get('key')!r}, not {key}")
    projects.append(entry)
json.dump(projects, sys.stdout)
PY
    projects_error="$deployment legion/projects/ at $head: $(last_line "$work/err")"
    return 1
  fi
  projects_state="read"
}

# project_pins <daemons|clients>: one line per project of $work/projects.json, tab-separated: the
# entry's key, then SKIP and why it runs no daemon, HOST for an entry whose daemon runs on a tmux
# host, RUNTIME and a runtime the gate does not know, NOIMAGE, or the images it reads: for daemons,
# READ and the image that runs its daemon; for clients, one IMAGE line per field it sets,
# daemonImage and image, each with its image.
project_pins() {
  python3 - "$work/projects.json" "$1" <<'PY'
import json
import sys

with open(sys.argv[1]) as handle:
    projects = json.load(handle)
clients = sys.argv[2] == "clients"
for entry in projects:
    key = entry["key"]
    if entry.get("replicas") == 0:
        print(f"{key}\tSKIP\treplicas: 0")
    elif entry.get("daemon") is False:
        print(f"{key}\tSKIP\tdaemon: false")
    elif entry.get("runtime") == "tmux":
        print(f"{key}\tHOST")
    elif entry.get("runtime") not in (None, "", "kubernetes"):
        print(f"{key}\tRUNTIME\t{entry.get('runtime')}")
    elif not (entry.get("daemonImage") or entry.get("image")):
        print(f"{key}\tNOIMAGE")
    elif clients:
        for field in ("daemonImage", "image"):
            if entry.get(field):
                print(f"{key}\tIMAGE\t{field}\t{entry[field]}")
    else:
        print(f"{key}\tREAD\t{entry.get('daemonImage') or entry.get('image')}")
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
# the repositoryRenames pairs of its legion/projects/ files, which render as each daemon's
# `repository_renames` and which the daemon's ghrepo.Renames reads
# (packages/daemon/internal/ghrepo/renames.go). It is the one place the transfer of sjawhar/legion
# to cybermonklabs/legion is recorded, so the gate keeps no copy of the pair. A record it cannot
# read leaves aliases_state failed, with the reason in aliases_error.
load_aliases() {
  [ "$aliases_state" = unread ] || return 0
  aliases_state=failed
  if ! load_projects; then
    aliases_error=$projects_error
    return 0
  fi
  if ! python3 - "$work/projects.json" >"$work/aliases" 2>"$work/err" <<'PY'; then
import json
import sys

with open(sys.argv[1]) as handle:
    projects = json.load(handle)
home = "cybermonklabs/legion"
for entry in projects:
    for pair in entry.get("repositoryRenames") or []:
        names = {str(pair.get("from", "")).lower(), str(pair.get("to", "")).lower()}
        if home in names:
            for name in names - {home, ""}:
                print(name)
PY
    aliases_error=$(last_line "$work/err")
    return 0
  fi
  aliases_state="read"
}

# compare_revision <label> <what it runs> <revision> <note>: compares <revision> with <commit> in
# cybermonklabs/legion and prints <label>'s line, <note> at its end. Each revision is compared once
# per run.
compare_revision() {
  local label=$1 runs=$2 rev=$3 via=$4 answer status behind_by
  if [ -s "$work/compared/$rev" ]; then
    answer=$(<"$work/compared/$rev")
  elif answer=$(gh api "repos/cybermonklabs/legion/compare/${commit}...${rev}" --jq '{status,behind_by}' 2>"$work/err"); then
    printf '%s' "$answer" >"$work/compared/$rev"
  else
    printf 'UNKNOWN: %s: cannot compare %s...%s in cybermonklabs/legion: %s\n' "$label" "$commit" "$rev" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! IFS=$'\t' read -r status behind_by < <(jq -r '[.status, .behind_by] | @tsv' <<<"$answer" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read GitHub'\''s comparison of %s...%s: %s\n' "$label" "$commit" "$rev" "$answer"
    unknown=$((unknown + 1))
    return
  fi
  case "$status:$behind_by" in
    ahead:0) printf 'PASS: %s runs %s (ahead of %s)%s\n' "$label" "$runs" "$commit" "$via" ;;
    identical:0) printf 'PASS: %s runs %s (identical to %s)%s\n' "$label" "$runs" "$commit" "$via" ;;
    behind:*)
      printf 'GATE SHUT: %s runs %s (behind %s by %s)%s\n' "$label" "$runs" "$commit" "$behind_by" "$via"
      shut=$((shut + 1))
      ;;
    diverged:*)
      printf 'GATE SHUT: %s runs %s (diverged from %s, behind by %s)%s\n' "$label" "$runs" "$commit" "$behind_by" "$via"
      shut=$((shut + 1))
      ;;
    *)
      printf 'UNKNOWN: %s: GitHub compared %s...%s as %s, behind by %s\n' "$label" "$commit" "$rev" "$status" "$behind_by"
      unknown=$((unknown + 1))
      ;;
  esac
}

# judge <key> <ref>: reads the pin, holds it to the source rule, compares it, and prints its line.
# A pin built under a name a recorded rename makes one repository with cybermonklabs/legion is a
# build of this repository, compared like any other; its line names the rename it was read by. A
# revision that is not 7 to 40 lowercase hex digits is UNKNOWN and never compared: GitHub's compare
# resolves any ref, so a label naming a branch or a tag (`main`) would compare as wherever that ref
# points now, not as the commit the image was built from.
judge() {
  local key=$1 ref=$2 repo via=""
  if ! read_pin "$key" "$ref"; then
    unknown=$((unknown + 1))
    return
  fi
  repo=${source#https://github.com/}
  repo=$(printf '%s' "${repo%.git}" | tr '[:upper:]' '[:lower:]')
  if [ "$repo" != cybermonklabs/legion ]; then
    load_aliases
    if [ "$aliases_state" = failed ]; then
      printf 'UNKNOWN: %s: built from %s, and the deployment'\''s repository renames could not be read: %s\n' "$key" "$source" "$aliases_error"
      unknown=$((unknown + 1))
      return
    fi
    if ! grep -qxF -- "$repo" "$work/aliases"; then
      printf 'GATE SHUT: %s runs an image built from %s\n' "$key" "$source"
      shut=$((shut + 1))
      return
    fi
    via="; built as $source, which legion/projects/'s repositoryRenames makes one repository with $legion_source"
  fi
  if ! [[ $revision =~ ^[0-9a-f]{7,40}$ ]]; then
    printf 'UNKNOWN: %s: %s names revision %s, which is no commit id\n' "$key" "$ref" "$revision"
    unknown=$((unknown + 1))
    return
  fi
  compare_revision "$key" "$revision" "$revision" "$via"
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

# vm_hub sets $hub to yes when a Go file of the deployment's envoy/ program at $head, other than a
# _test.go, has a line that calls NewNatsHub( and is not a // comment, and to no when the listing
# and every read answered and no file does; it returns 0. A listing or a read that fails, or a
# listing with no Go file, prints UNKNOWN and returns 1. The pin is never the evidence: the hub's
# removal deletes its config keys too, so an absent listenerImage cannot tell a gone hub from a
# stack file that lost its pin.
vm_hub() {
  local name type files=0
  if ! deployment_dir envoy "$work/envoy.ls"; then
    printf 'UNKNOWN: envoy: cannot list %s envoy/ at %s: %s\n' "$deployment" "$head" "$(first_line "$work/err")"
    return 1
  fi
  hub=no
  while IFS=$'\t' read -r name type; do
    [ "$type" = file ] && [[ $name == *.go ]] && [[ $name != *_test.go ]] || continue
    files=$((files + 1))
    if ! deployment_file "envoy/$name" "$work/envoy.go"; then
      printf 'UNKNOWN: envoy: cannot read %s envoy/%s at %s: %s\n' "$deployment" "$name" "$head" "$(first_line "$work/err")"
      return 1
    fi
    if awk '!/^[[:space:]]*\/\// && index($0, "NewNatsHub(") { found = 1 } END { exit !found }' "$work/envoy.go"; then
      hub=yes
    fi
  done <"$work/envoy.ls"
  if [ "$files" -eq 0 ]; then
    printf 'UNKNOWN: envoy: %s envoy/ at %s lists no Go file to read the VM hub from\n' "$deployment" "$head"
    return 1
  fi
}

# in_image <ref> <entrypoint> [<arg>...]: the output of <entrypoint> run in a container of <ref>
# with no network, which pulls <ref> when docker does not hold it; docker's error into $work/err.
# Each command is run once per image and its answer kept, a refusal included.
in_image() {
  local answer
  answer="$work/runs/$(printf '%s\0' "$@" | sha256sum | cut -c1-32)"
  if [ ! -e "$answer.status" ]; then
    local status=0
    docker run --rm --network none --entrypoint "$2" "$1" "${@:3}" </dev/null >"$answer" 2>"$answer.err" || status=$?
    printf '%s' "$status" >"$answer.status"
  fi
  cp "$answer.err" "$work/err"
  [ "$(<"$answer.status")" = 0 ] && cat "$answer"
}

# release_commit <package> <version>: sets $tag to <package>-v<version>, the release tag of that
# version, and $tag_commit to the commit it names in cybermonklabs/legion; GitHub's error into
# $work/err.
release_commit() {
  tag="$1-v$2"
  if [ -s "$work/tags/$tag" ]; then
    tag_commit=$(<"$work/tags/$tag")
    return 0
  fi
  tag_commit=$(gh api "repos/cybermonklabs/legion/commits/refs/tags/$tag" --jq .sha 2>"$work/err") || return 1
  if ! [[ $tag_commit =~ ^[0-9a-f]{40}$ ]]; then
    echo "GitHub answered $tag_commit, which is no commit id" >"$work/err"
    return 1
  fi
  printf '%s' "$tag_commit" >"$work/tags/$tag"
}

# judge_release <label> <package> <version> [<note>]: compares the commit <package>'s release
# <version> was tagged at, and prints <label>'s line. A version that is not one exact release
# version, or one no release tag names, is UNKNOWN.
judge_release() {
  local label=$1 package=$2 version=$3 note=${4:-}
  if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'UNKNOWN: %s: %s %s is no exact release version\n' "$label" "$package" "$version"
    unknown=$((unknown + 1))
    return
  fi
  if ! release_commit "$package" "$version"; then
    printf 'UNKNOWN: %s: %s %s has no release tag %s in cybermonklabs/legion: %s\n' "$label" "$package" "$version" "$tag" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  compare_revision "$label" "$package $version from $tag at $tag_commit" "$tag_commit" "$note"
}

# image_clients <label> <ref>: the two clients of a worker image, pi-envoy and claude-envoy.
image_clients() {
  image_pi_envoy "$1" "$2"
  image_claude_envoy "$1" "$2"
}

# image_pi_envoy <label> <ref>: the pi-envoy root a pod loads, by its release version.
image_pi_envoy() {
  local label=$1 ref=$2 manifest version
  if ! manifest=$(in_image "$ref" cat /opt/legion/pi-envoy/package.json); then
    printf 'UNKNOWN: %s: cannot read /opt/legion/pi-envoy/package.json in %s: %s\n' "$label" "$ref" "$(last_line "$work/err")"
    unknown=$((unknown + 1))
  elif ! version=$(jq -er 'select(.name == "@sjawhar/pi-envoy") | .version | strings' <<<"$manifest" 2>/dev/null); then
    printf 'UNKNOWN: %s: /opt/legion/pi-envoy/package.json in %s is no @sjawhar/pi-envoy manifest with a version\n' "$label" "$ref"
    unknown=$((unknown + 1))
  else
    judge_release "$label" pi-envoy "$version"
  fi
}

# image_claude_envoy <label> <ref>: the claude-envoy root a Claude worker loads, by the commit of
# the image's legion, which the image builds it from.
image_claude_envoy() {
  local label=$1 ref=$2 manifest line
  if ! manifest=$(in_image "$ref" cat /opt/legion/claude-envoy/package.json); then
    printf 'UNKNOWN: %s: cannot read /opt/legion/claude-envoy/package.json in %s: %s\n' "$label" "$ref" "$(last_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! jq -e '.name == "@sjawhar/claude-envoy"' <<<"$manifest" >/dev/null 2>&1; then
    printf 'UNKNOWN: %s: /opt/legion/claude-envoy/package.json in %s is no @sjawhar/claude-envoy manifest\n' "$label" "$ref"
    unknown=$((unknown + 1))
    return
  fi
  if ! line=$(in_image "$ref" /opt/legion/bin/legion version); then
    printf 'UNKNOWN: %s: cannot run /opt/legion/bin/legion version in %s: %s\n' "$label" "$ref" "$(last_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  line=${line%%$'\n'*}
  if ! [[ $line =~ ^legion\ [^\ ]+\ commit\ ([0-9a-f]{40})$ ]]; then
    printf 'UNKNOWN: %s: the legion in %s answers version with "%s", which names no commit\n' "$label" "$ref" "$line"
    unknown=$((unknown + 1))
    return
  fi
  compare_revision "$label" "claude-envoy built at ${BASH_REMATCH[1]}, the commit of the image's legion" "${BASH_REMATCH[1]}" ""
}

# dotfiles_clients: the pi-envoy and claude-envoy pins of this developer box's ~/.dotfiles.
dotfiles_clients() {
  local root="$HOME/.dotfiles" tree outside="" pin enabled marketplace repo ref plugin package range served
  local label="dotfiles omp/plugins/package.json"
  for tree in "$HOME/.omp/plugins" "$HOME"/.omp/profiles/*/plugins; do
    [ -e "$tree/node_modules/@sjawhar/pi-envoy/package.json" ] || continue
    [ "$(readlink -f "$tree")" = "$(readlink -f "$root/omp/plugins")" ] || outside+="${outside:+, }$tree"
  done
  if [ -n "$outside" ]; then
    printf 'UNKNOWN: %s: @sjawhar/pi-envoy is installed outside ~/.dotfiles, in %s\n' "$label" "$outside"
    unknown=$((unknown + 1))
  elif ! pin=$(jq -er '.dependencies["@sjawhar/pi-envoy"] | strings' "$root/omp/plugins/package.json" 2>"$work/err"); then
    printf 'UNKNOWN: %s: it pins no @sjawhar/pi-envoy version this gate can read%s\n' "$label" "$(sed -n '1s/^/: /p' "$work/err")"
    unknown=$((unknown + 1))
  else
    judge_release "$label" pi-envoy "$pin"
  fi

  label="dotfiles .claude/settings.json"
  if [ -e "$HOME/.claude/plugins/installed_plugins.json" ] &&
    jq -e '.plugins // {} | keys | any(startswith("claude-envoy@"))' "$HOME/.claude/plugins/installed_plugins.json" >/dev/null 2>&1; then
    printf 'UNKNOWN: %s: claude-envoy is installed outside ~/.dotfiles, in %s\n' "$label" "$HOME/.claude/plugins"
    unknown=$((unknown + 1))
    return
  fi
  if ! enabled=$(jq -r '.enabledPlugins["claude-envoy@legion-plugins"] // false' "$root/.claude/settings.json" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read it: %s\n' "$label" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ "$enabled" != true ]; then
    printf 'SKIPPED: %s enables no claude-envoy@legion-plugins\n' "$label"
    return
  fi
  marketplace=$(jq -c '.extraKnownMarketplaces["legion-plugins"].source // {}' "$root/.claude/settings.json")
  if [ "$(jq -r '.source // ""' <<<"$marketplace")" != github ] || ! repo=$(jq -er '.repo | strings' <<<"$marketplace"); then
    printf 'UNKNOWN: %s: the marketplace legion-plugins it names is %s, which is no GitHub repository this gate reads\n' "$label" "$marketplace"
    unknown=$((unknown + 1))
    return
  fi
  ref=$(jq -r '.ref // ""' <<<"$marketplace")
  if ! gh api -H "Accept: application/vnd.github.raw+json" "repos/$repo/contents/.claude-plugin/marketplace.json${ref:+?ref=$ref}" >"$work/marketplace.json" 2>"$work/err"; then
    printf 'UNKNOWN: %s: cannot read %s .claude-plugin/marketplace.json%s: %s\n' "$label" "$repo" "${ref:+ at $ref}" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! plugin=$(jq -ec 'first(.plugins[] | select(.name == "claude-envoy") | .source)' "$work/marketplace.json" 2>/dev/null) ||
    [ "$(jq -r '.source? // ""' <<<"$plugin")" != npm ] ||
    [ "$(jq -r '.package? // ""' <<<"$plugin")" != "@sjawhar/claude-envoy" ]; then
    printf 'UNKNOWN: %s: %s .claude-plugin/marketplace.json serves claude-envoy as %s, not as the npm package @sjawhar/claude-envoy\n' "$label" "$repo" "${plugin:-nothing}"
    unknown=$((unknown + 1))
    return
  fi
  range=$(jq -r '.version // "latest"' <<<"$plugin")
  if ! served=$(npm view "@sjawhar/claude-envoy@$range" version --json 2>"$work/err"); then
    printf 'UNKNOWN: %s: npm cannot resolve @sjawhar/claude-envoy@%s: %s\n' "$label" "$range" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  served=$(jq -r 'if type == "array" then .[] else . end | strings' <<<"$served" 2>/dev/null | sort -V | tail -n 1)
  if [ -z "$served" ]; then
    printf 'UNKNOWN: %s: npm matches no version of @sjawhar/claude-envoy to %s\n' "$label" "$range"
    unknown=$((unknown + 1))
    return
  fi
  judge_release "$label" claude-envoy "$served" "; the version legion-plugins serves for $range"
}

# project_lines <daemons|clients>: reads legion/projects/ and prints each project's line, judging
# the images the mode reads (judge for a daemon, image_clients for a client). A deployment that
# lists no image to read, with nothing UNKNOWN already, is UNKNOWN too: no pin read is no pass.
project_lines() {
  local key what detail ref images=0
  if ! load_projects; then
    printf 'UNKNOWN: %s\n' "$projects_error"
    unknown=$((unknown + 1))
    return
  fi
  if ! project_pins "$1" >"$work/pins" 2>"$work/err"; then
    printf 'UNKNOWN: %s legion/projects/ at %s: %s\n' "$deployment" "$head" "$(last_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  while IFS=$'\t' read -r key what detail ref; do
    case "$what" in
      SKIP) printf 'SKIPPED: %s runs no daemon (%s)\n' "$key" "$detail" ;;
      READ) images=$((images + 1)) && judge "$key" "$detail" ;;
      IMAGE) images=$((images + 1)) && image_clients "$key $detail" "$ref" ;;
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
    esac
  done <"$work/pins"
  if [ "$images" -eq 0 ] && [ "$unknown" -eq 0 ]; then
    echo "UNKNOWN: legion/projects/ lists no running daemon, so no image was read"
    unknown=1
  fi
}

case "$mode" in
  listeners)
    if vm_hub; then
      if [ "$hub" = yes ]; then
        listener envoy envoy "${project_prefix}-envoy:listenerImage"
      else
        printf 'NO VM HUB: envoy/ at %s declares no gcp:NatsHub\n' "$head"
      fi
    else
      unknown=$((unknown + 1))
    fi
    listener envoy-cluster envoy-cluster "${project_prefix}-envoy-cluster:envoyImage"
    # dispatch's listener runs listenerImage and nothing else: the deployment's dispatch/main.go
    # requires that key (loadSettings) and pinListenerImage sets the listener's container from it,
    # leaving envoyImage to the Dispatch server. A stack that sets none runs no listener this gate
    # can read, so it is UNKNOWN rather than a reading of the server's image.
    listener dispatch dispatch "${project_prefix}-dispatch:listenerImage"
    ;;
  clients)
    project_lines clients
    dotfiles_clients
    ;;
  daemons)
    project_lines daemons
    ;;
esac
verdict
