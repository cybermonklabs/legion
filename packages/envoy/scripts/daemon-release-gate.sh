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
# name, and so is an entry on any runtime but `kubernetes`, the default. A field of the wrong type (a
# `replicas` that is no count, a `daemon` that is no boolean) and an image that is no plain
# reference are UNKNOWN by name, never a project skipped or an image cut short.
#
# With --listeners it reads the listener pins instead: envoy-cluster/Pulumi.prod.yaml
# `envoyImage`, dispatch/Pulumi.prod.yaml `listenerImage`, which that stack's program sets on its
# listener in place of `envoyImage`, the Dispatch server's (the deployment's dispatch/main.go,
# pinListenerImage), and, when the deployment declares its VM hub, envoy/Pulumi.prod.yaml
# `listenerImage`, which only that hub's listener reads. Whether it declares the VM hub is decided
# from the envoy/ program itself, never from the pin: the gate reads every Go file a build of
# envoy/ compiles from the deployment repository (envoy/ and its subdirectories, and each
# directory a local `replace` of envoy/go.mod names, without _test.go files and the testdata/, _
# and . directories go build leaves out), and the program declares the hub when its code, outside
# comments and strings, uses the identifier `NewNatsHub` anywhere but as the name of the func
# that defines it. Declared, the pin is read, and a stack that sets none is UNKNOWN. Declared by no
# file, every file read, the gate prints `NO VM HUB: envoy/ at <sha> declares no gcp:NatsHub` and
# reads no VM pin. A listing, a read or a go.mod that fails, a listing GitHub truncated, a program
# with no Go file and a file the scan cannot read as Go are UNKNOWN, so a missing pin never reads as
# a hub that is gone. A dispatch stack that sets no `listenerImage` is UNKNOWN: its deployment
# requires that key, and the listener runs no other image.
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
#   - on this developer box, the `@sjawhar/pi-envoy` pin of ~/.dotfiles/omp/plugins/package.json
#     and, where it differs from the pin, the version installed beside it, each resolved to its
#     release tag; each claude-envoy@legion-plugins install the dotfiles' Claude Code plugin record
#     (~/.dotfiles/.claude/plugins/installed_plugins.json) holds; and the claude-envoy version the
#     marketplace `legion-plugins` serves to ~/.dotfiles/.claude/settings.json: the marketplace's
#     `.claude-plugin/marketplace.json`, read from the GitHub repository settings.json names for
#     it, gives one npm package and a version range, the highest version npm matches to the range
#     is the served one, and `claude-envoy-v<version>` its release tag. A settings.json that
#     enables no `claude-envoy@legion-plugins` is SKIPPED. A pi-envoy installed in an Oh My Pi
#     plugin tree whose copy is not ~/.dotfiles/omp/plugins' (~/.omp/plugins, or a profile's), a
#     claude-envoy installed in ~/.claude/plugins or from another marketplace, and a plugin record
#     it cannot read are UNKNOWN: the dotfiles' pins say nothing about them. A session started
#     before a pin moved still runs the plugin it loaded; the gate reads pins and installs, not
#     processes.
#   Each commit is compared with <commit> as a daemon's revision is. A version that is no exact
#   release version, or one no release tag names, is UNKNOWN.
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
# host, RUNTIME and a runtime the gate does not know, NOIMAGE, INVALID and the field it cannot take,
# or the images it reads: for daemons, READ and the image that runs its daemon; for clients, one
# IMAGE line per field it sets, daemonImage and image, each with its image. A field of the wrong type
# (a replicas that is no count, as YAML's false would equal 0; a daemon that is no boolean) or an
# image that is no plain reference (a tab or space in it would cut the line, and the gate would read
# another image) is INVALID, never a project skipped or an image shortened.
project_pins() {
  python3 - "$work/projects.json" "$1" <<'PY'
import json
import re
import sys

with open(sys.argv[1]) as handle:
    projects = json.load(handle)
clients = sys.argv[2] == "clients"
reference = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/:@+-]*")
for entry in projects:
    key = entry["key"]
    replicas, daemon = entry.get("replicas"), entry.get("daemon")
    if "replicas" in entry and (isinstance(replicas, bool) or not isinstance(replicas, int) or replicas < 0):
        print(f"{key}\tINVALID\treplicas {json.dumps(replicas)}, which is no count of replicas")
        continue
    if "daemon" in entry and not isinstance(daemon, bool):
        print(f"{key}\tINVALID\tdaemon {json.dumps(daemon)}, which is neither true nor false")
        continue
    images = {field: entry[field] for field in ("daemonImage", "image") if entry.get(field) not in (None, "")}
    bad = [field for field, image in images.items() if not (isinstance(image, str) and reference.fullmatch(image))]
    if replicas == 0:
        print(f"{key}\tSKIP\treplicas: 0")
    elif daemon is False:
        print(f"{key}\tSKIP\tdaemon: false")
    elif entry.get("runtime") == "tmux":
        print(f"{key}\tHOST")
    elif entry.get("runtime") not in (None, "", "kubernetes"):
        runtime = entry["runtime"]
        print(f"{key}\tRUNTIME\t{runtime if isinstance(runtime, str) and runtime.isprintable() else json.dumps(runtime)}")
    elif bad:
        print(f"{key}\tINVALID\t{bad[0]} {json.dumps(images[bad[0]])}, which is no image reference")
    elif not images:
        print(f"{key}\tNOIMAGE")
    elif clients:
        for field, image in images.items():
            print(f"{key}\tIMAGE\t{field}\t{image}")
    else:
        print(f"{key}\tREAD\t{images.get('daemonImage') or images['image']}")
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

# vm_hub sets $hub to yes when the deployment's envoy/ program at $head uses NewNatsHub, to no when
# every Go file of it was read and none does, and returns 0. The program is every Go file a build
# of envoy/ compiles from this repository: envoy/ and its subdirectories, and each directory a local
# `replace` of envoy/go.mod names, each without its _test.go files and its testdata/, _ and .
# directories, which go build leaves out. A use is the identifier NewNatsHub anywhere in code but a
# comment or a string and but the name of a func it declares, so `NewNatsHub (`, a comment between
# it and its call, a reference that calls it later and a call from a helper package all count. A
# listing, a read or a go.mod that fails, a listing GitHub truncated, a program with no Go file and
# a file the scan cannot read as Go (an unterminated comment or string) print UNKNOWN and return 1.
# The pin is never the evidence: the hub's removal deletes its config keys too, so an absent
# listenerImage cannot tell a gone hub from a stack file that lost its pin.
vm_hub() {
  local root path type truncated files=0
  if ! deployment_file envoy/go.mod "$work/go.mod"; then
    printf 'UNKNOWN: envoy: cannot read %s envoy/go.mod at %s: %s\n' "$deployment" "$head" "$(first_line "$work/err")"
    return 1
  fi
  if ! python3 - "$work/go.mod" >"$work/go.roots" 2>"$work/err" <<'PY'; then
import posixpath
import re
import sys

text = re.sub(r"//[^\n]*", "", open(sys.argv[1]).read())
roots = ["envoy"]
blocks = re.findall(r"^replace\s*\(([^)]*)\)", text, re.M)
lines = re.findall(r"^replace\s+([^(\s][^\n]*)$", text, re.M)
for line in lines + [entry for block in blocks for entry in block.splitlines()]:
    if "=>" not in line:
        continue
    target = line.split("=>", 1)[1].split()
    if len(target) == 1 and target[0].startswith(("./", "../")):
        root = posixpath.normpath(posixpath.join("envoy", target[0]))
        if root == ".." or root.startswith("../") or root.startswith("/"):
            sys.exit(f"envoy/go.mod replaces a module with {target[0]}, outside this repository")
        if root not in roots:
            roots.append(root)
print("\n".join(roots))
PY
    printf 'UNKNOWN: envoy: cannot read %s envoy/go.mod at %s: %s\n' "$deployment" "$head" "$(last_line "$work/err")"
    return 1
  fi
  rm -rf "$work/go"
  mkdir "$work/go"
  while read -r root; do
    if ! gh api "repos/$deployment/git/trees/$head:$root?recursive=1" \
      --jq '(.truncated | tostring), (.tree[] | [.path, .type] | @tsv)' >"$work/go.tree" 2>"$work/err"; then
      printf 'UNKNOWN: envoy: cannot list %s %s/ at %s: %s\n' "$deployment" "$root" "$head" "$(first_line "$work/err")"
      return 1
    fi
    truncated=$(head -n 1 "$work/go.tree")
    if [ "$truncated" != false ]; then
      printf 'UNKNOWN: envoy: GitHub listed %s %s/ at %s truncated (%s), so not every Go file can be read\n' "$deployment" "$root" "$head" "$truncated"
      return 1
    fi
    while IFS=$'\t' read -r path type; do
      [ "$type" = blob ] && [[ $path == *.go ]] && [[ $path != *_test.go ]] || continue
      [[ /$path != */testdata/* && /$path != */_* && /$path != */.* ]] || continue
      files=$((files + 1))
      mkdir -p "$work/go/$root/$(dirname "$path")"
      if ! deployment_file "$root/$path" "$work/go/$root/$path"; then
        printf 'UNKNOWN: envoy: cannot read %s %s/%s at %s: %s\n' "$deployment" "$root" "$path" "$head" "$(first_line "$work/err")"
        return 1
      fi
    done < <(tail -n +2 "$work/go.tree")
  done <"$work/go.roots"
  if [ "$files" -eq 0 ]; then
    printf 'UNKNOWN: envoy: %s envoy/ at %s lists no Go file to read the VM hub from\n' "$deployment" "$head"
    return 1
  fi
  if ! python3 - "$work/go" >"$work/go.hub" 2>"$work/err" <<'PY'; then
import os
import re
import sys


def code(text, name):
    """text with every comment and string blanked out, newlines kept."""
    out, i, n = [], 0, len(text)
    while i < n:
        if text.startswith("//", i):
            j = text.find("\n", i)
            j = n if j < 0 else j
        elif text.startswith("/*", i):
            j = text.find("*/", i + 2)
            if j < 0:
                sys.exit(f"{name} has a /* comment with no end")
            j += 2
        elif text[i] == "`":
            j = text.find("`", i + 1)
            if j < 0:
                sys.exit(f"{name} has a raw string with no end")
            j += 1
        elif text[i] in "\"'":
            j = i + 1
            while j < n and text[j] not in (text[i], "\n"):
                j += 2 if text[j] == "\\" else 1
            if j >= n or text[j] != text[i]:
                sys.exit(f"{name} has a string with no end")
            j += 1
        else:
            out.append(text[i])
            i += 1
            continue
        out.append(re.sub(r"[^\n]", " ", text[i:j]))
        i = j
    return "".join(out)


def declared_name(before):
    """whether the code before an identifier makes it the name of a func being declared."""
    before = before.rstrip()
    if before.endswith(")"):
        depth, i = 0, len(before) - 1
        while i >= 0:
            depth += {")": 1, "(": -1}.get(before[i], 0)
            if depth == 0:
                break
            i -= 1
        before = before[:i].rstrip()
    return re.search(r"(?<!\w)func$", before) is not None


root = sys.argv[1]
for directory, _, files in sorted(os.walk(root)):
    for file in sorted(files):
        path = os.path.join(directory, file)
        with open(path, encoding="utf-8", errors="replace") as handle:
            source = code(handle.read(), os.path.relpath(path, root))
        if any(not declared_name(source[: use.start()]) for use in re.finditer(r"(?<!\w)NewNatsHub(?!\w)", source)):
            print("yes")
            sys.exit(0)
print("no")
PY
    printf 'UNKNOWN: envoy: cannot read the Go files of %s envoy/ at %s: %s\n' "$deployment" "$head" "$(last_line "$work/err")"
    return 1
  fi
  hub=$(<"$work/go.hub")
}

# in_image <ref> <entrypoint> [<arg>...]: the output of <entrypoint> run in a container of <ref>
# with no network, which pulls <ref> when docker does not hold it; docker's answer into $work/err,
# the last line of its error that is not blank and not its `Run 'docker run --help'` hint. Each
# command is run once per image and its answer kept, a refusal included.
in_image() {
  local answer
  answer="$work/runs/$(printf '%s\0' "$@" | sha256sum | cut -c1-32)"
  if [ ! -e "$answer.status" ]; then
    local status=0
    docker run --rm --network none --entrypoint "$2" "$1" "${@:3}" </dev/null >"$answer" 2>"$answer.err" || status=$?
    printf '%s' "$status" >"$answer.status"
  fi
  awk '!/^Run \047/ && NF { line = $0 } END { print line }' "$answer.err" >"$work/err"
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
# version (a pre-release one counts; a range or a build suffix does not), or one no release tag
# names, is UNKNOWN.
judge_release() {
  local label=$1 package=$2 version=$3 note=${4:-}
  if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
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

# installed_claude_envoy <installed_plugins.json>: one `<plugin>\t<version>` line per install of a
# claude-envoy@<marketplace> the Claude Code plugin record holds, in either of its two shapes (a
# plugin's value an install, or a list of them); a record that cannot be read as one fails, naming
# why.
installed_claude_envoy() {
  python3 - "$1" <<'PY'
import json
import sys

try:
    with open(sys.argv[1]) as handle:
        record = json.load(handle)
except (OSError, ValueError) as error:
    sys.exit(f"{sys.argv[1]} cannot be read as JSON: {error}")
if not isinstance(record, dict) or not isinstance(record.get("plugins"), dict):
    sys.exit(f"{sys.argv[1]} holds no plugins object")
for name, installs in record["plugins"].items():
    if not name.startswith("claude-envoy@"):
        continue
    installs = [installs] if isinstance(installs, dict) else installs
    if not isinstance(installs, list) or not installs:
        sys.exit(f"{sys.argv[1]} records {name} as no install")
    for install in installs:
        version = install.get("version") if isinstance(install, dict) else None
        if not isinstance(version, str) or not version.isprintable():
            sys.exit(f"{sys.argv[1]} records an install of {name} with no version")
        print(f"{name}\t{version}")
PY
}

# dotfiles_clients: the pi-envoy and claude-envoy of this developer box's ~/.dotfiles: the pins,
# and the copies installed from them.
dotfiles_clients() {
  if [ -z "${HOME:-}" ]; then
    echo "UNKNOWN: dotfiles: HOME is not set, so this box's ~/.dotfiles cannot be found"
    unknown=$((unknown + 1))
    return
  fi
  local root="$HOME/.dotfiles" tree own home outside="" pin installed label
  local package="$HOME/.dotfiles/omp/plugins/node_modules/@sjawhar/pi-envoy"
  label="dotfiles omp/plugins/package.json"
  # Each copy an Oh My Pi plugin tree holds must be the dotfiles' own, a directory inside the
  # dotfiles once every link is resolved: `omp plugin link` puts a link to a checkout at that path,
  # and a tree linked to the dotfiles would carry it inside them.
  home=$(readlink -f "$root" || true)
  own=$(readlink -f "$package" || true)
  if [ -e "$package" ] && [[ $own != "$home"/* ]]; then
    outside="$root/omp/plugins, a link to ${own:-a path that does not resolve}"
  fi
  for tree in "$HOME/.omp/plugins" "$HOME"/.omp/profiles/*/plugins; do
    [ -e "$tree/node_modules/@sjawhar/pi-envoy/package.json" ] || continue
    [ "$(readlink -f "$tree/node_modules/@sjawhar/pi-envoy" || true)" = "$own" ] || outside+="${outside:+, }$tree"
  done
  if [ -n "$outside" ]; then
    printf 'UNKNOWN: %s: @sjawhar/pi-envoy is installed outside ~/.dotfiles, in %s\n' "$label" "$outside"
    unknown=$((unknown + 1))
  elif ! pin=$(jq -er '.dependencies["@sjawhar/pi-envoy"] | strings' "$root/omp/plugins/package.json" 2>"$work/err"); then
    printf 'UNKNOWN: %s: it pins no @sjawhar/pi-envoy version this gate can read%s\n' "$label" "$(sed -n '1s/^/: /p' "$work/err")"
    unknown=$((unknown + 1))
  else
    judge_release "$label" pi-envoy "$pin"
    # The copy installed from the pin is what a new session loads; one left behind the pin by an
    # install that did not finish is read on its own.
    if [ -e "$package" ]; then
      label="dotfiles omp/plugins installed @sjawhar/pi-envoy"
      if ! installed=$(jq -er 'select(.name == "@sjawhar/pi-envoy") | .version | strings' "$package/package.json" 2>/dev/null); then
        printf 'UNKNOWN: %s: %s/package.json is no @sjawhar/pi-envoy manifest with a version\n' "$label" "$package"
        unknown=$((unknown + 1))
      elif [ "$installed" != "$pin" ]; then
        judge_release "$label" pi-envoy "$installed" "; the pin names $pin"
      fi
    fi
  fi
  dotfiles_claude_envoy "$root"
}

# dotfiles_claude_envoy <dotfiles>: the claude-envoy the dotfiles' Claude Code settings enable: each
# copy installed in the dotfiles' Claude Code configuration, and the version the marketplace
# legion-plugins serves to a new install. A copy recorded in any other Claude Code configuration,
# ~/.claude or the CLAUDE_CONFIG_DIR this run is given, is UNKNOWN.
dotfiles_claude_envoy() {
  local root=$1 label="dotfiles .claude/settings.json" record name version enabled others marketplace repo ref plugin range served
  for record in "$HOME/.claude/plugins/installed_plugins.json" ${CLAUDE_CONFIG_DIR:+"$CLAUDE_CONFIG_DIR/plugins/installed_plugins.json"}; do
    [ -e "$record" ] || continue
    [ "$(readlink -f "$record")" != "$(readlink -f "$root/.claude/plugins/installed_plugins.json" || true)" ] || continue
    if ! installed_claude_envoy "$record" >"$work/claude.outside" 2>"$work/err"; then
      printf 'UNKNOWN: %s: %s\n' "$label" "$(last_line "$work/err")"
      unknown=$((unknown + 1))
      return
    fi
    if [ -s "$work/claude.outside" ]; then
      printf 'UNKNOWN: %s: claude-envoy is installed outside ~/.dotfiles, in %s\n' "$label" "$(dirname "$record")"
      unknown=$((unknown + 1))
      return
    fi
  done
  if ! enabled=$(jq -r '.enabledPlugins["claude-envoy@legion-plugins"] // false | tostring' "$root/.claude/settings.json" 2>"$work/err") ||
    ! others=$(jq -r '.enabledPlugins // {} | to_entries | map(select(.key | startswith("claude-envoy@")) | select(.key != "claude-envoy@legion-plugins") | select(.value != false) | .key) | join(", ")' "$root/.claude/settings.json" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read it: %s\n' "$label" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ -n "$others" ]; then
    printf 'UNKNOWN: %s: it enables %s, from a marketplace this gate does not read\n' "$label" "$others"
    unknown=$((unknown + 1))
    return
  fi
  if [ -e "$root/.claude/plugins/installed_plugins.json" ]; then
    if ! installed_claude_envoy "$root/.claude/plugins/installed_plugins.json" >"$work/claude.installed" 2>"$work/err"; then
      printf 'UNKNOWN: %s: %s\n' "$label" "$(last_line "$work/err")"
      unknown=$((unknown + 1))
      return
    fi
    while IFS=$'\t' read -r name version; do
      if [ "$name" != claude-envoy@legion-plugins ]; then
        printf 'UNKNOWN: %s: %s is installed, from a marketplace this gate does not read\n' "$label" "$name"
        unknown=$((unknown + 1))
      else
        judge_release "dotfiles .claude/plugins installed claude-envoy" claude-envoy "$version"
      fi
    done <"$work/claude.installed"
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
  if ! plugin=$(jq -ec '[.plugins[] | select(.name == "claude-envoy")] | if length == 1 then .[0].source else error("\(length) entries name claude-envoy") end' "$work/marketplace.json" 2>"$work/err") ||
    [ "$(jq -r '.source? // ""' <<<"$plugin")" != npm ] ||
    [ "$(jq -r '.package? // ""' <<<"$plugin")" != "@sjawhar/claude-envoy" ]; then
    printf 'UNKNOWN: %s: %s .claude-plugin/marketplace.json serves claude-envoy as %s, not as one entry of the npm package @sjawhar/claude-envoy\n' "$label" "$repo" "${plugin:-$(first_line "$work/err")}"
    unknown=$((unknown + 1))
    return
  fi
  range=$(jq -r '.version // "latest"' <<<"$plugin")
  if ! served=$(npm view "@sjawhar/claude-envoy@$range" version --json 2>"$work/err"); then
    printf 'UNKNOWN: %s: npm cannot resolve @sjawhar/claude-envoy@%s: %s\n' "$label" "$range" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! served=$(jq -er 'if type == "string" then . elif type == "array" and length > 0 and all(.[]; type == "string") then .[] else error("no list of versions") end' <<<"$served" 2>/dev/null); then
    printf 'UNKNOWN: %s: npm answers no version of @sjawhar/claude-envoy for %s\n' "$label" "$range"
    unknown=$((unknown + 1))
    return
  fi
  served=$(sort -V <<<"$served" | tail -n 1)
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
      INVALID)
        printf 'UNKNOWN: %s names %s\n' "$key" "$detail"
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
