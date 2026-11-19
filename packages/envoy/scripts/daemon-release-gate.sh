#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.sh --deployment <owner/repo> [--listeners] <commit>
#
# Says whether every production reader of a message name runs a build at or after <commit> of
# cybermonklabs/legion, the reading a pull request quotes before it moves a producer onto the new
# form of a name (dispatch://INFRA-3862, plan C's rollout). A reader on an older image cannot read
# the new form: a Legion daemon built before t-c-5 takes an issue event named in the new form for
# another project's and acknowledges it with no fact, so moving a producer early stops the fleet's
# workflow in silence.
#
# The deployment repository, the one whose Pulumi stacks pin these images, is the operator's to
# name with --deployment: the gate compiles in no deployment's name. Each stack's config keys are
# under its Pulumi project's name, which the gate reads from that stack's own Pulumi.yaml (`name:`)
# and never guesses: a stack whose Pulumi.yaml is missing or declares no name is UNKNOWN, by name.
#
# The deployment's Legion projects are the files of legion/projects/ (`<nn>-<KEY>.yaml`, one mapping
# of a project's settings each, read in file-name order), as the deployment's legion program reads
# them. Only a deployment with no such directory still declares them in legion/Pulumi.prod.yaml's
# projects list, as that program did before the files (load_entries).
#
# Without --listeners it reads the image that runs each Legion daemon: for each project, its
# `daemonImage` when set, else its `image` (legion/README.md's `daemonImage` row). A project that runs
# no daemon (`replicas: 0`, or `daemon: false`) is printed SKIPPED, by name. An entry at `runtime:
# tmux` runs its daemon on the project's host VM from `host.legionWorker`'s legion, not from its
# `image` (deployment legion/host.go, checkHost), and the gate reads no image for it: it is UNKNOWN
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
# records as the same repository (the repositoryRenames pairs of its Legion projects, which the
# daemon reads as `repository_renames`: sjawhar/legion is this repository under
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
# `GATE SHUT` (exit 1) when any pin shut it, and `UNKNOWN` (exit 3) otherwise. A usage error, a
# missing --deployment among them, exits 2 and reads nothing.
#
# It needs bash, gh (signed in with read access to the deployment repository and cybermonklabs/legion),
# docker with buildx, jq, base64 and python3 with PyYAML. Docker must be able to read each pin's
# registry: for ghcr.io, a login with a token holding read:packages, made into a config directory of
# your own so the shared one is untouched:
#   export DOCKER_CONFIG=$(mktemp -d)
#   gh auth token | docker login ghcr.io -u x --password-stdin
# A pin docker cannot read prints UNKNOWN with docker's own answer, never GATE OPEN.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "daemon-release-gate.sh: $1" >&2
  echo "usage: daemon-release-gate.sh --deployment <owner/repo> [--listeners] <commit>" >&2
  echo "  <owner/repo> is the deployment repository whose Pulumi stacks pin the readers' images" >&2
  echo "  <commit> is the cybermonklabs/legion commit, 7 to 40 hex digits, every reader must run at or after" >&2
  exit 2
}

mode=daemons
deployment=""
while [ "$#" -gt 1 ]; do
  case "$1" in
    --listeners) mode=listeners; shift ;;
    --deployment) deployment=$2; shift 2 ;;
    --deployment=*) deployment=${1#--deployment=}; shift ;;
    *) break ;;
  esac
done
# A `--deployment` with its value left out takes the commit as the deployment and leaves no commit.
[ "$#" -gt 0 ] || usage "no <commit> follows the flags${deployment:+; --deployment took ${deployment} as its <owner/repo>}"
[ "$#" -eq 1 ] && [[ $1 =~ ^[0-9a-f]{7,40}$ ]] || usage
[ -n "$deployment" ] || usage "--deployment <owner/repo> is required: the deployment repository whose stacks the gate reads"
[[ $deployment =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage "--deployment ${deployment} is not <owner>/<repo>"
readonly commit=$1 deployment
readonly legion_source="https://github.com/cybermonklabs/legion"

work=$(mktemp -d)
readonly work
trap 'rm -rf "$work"' EXIT

shut=0
unknown=0
compared=0

first_line() { head -n 1 "$1"; }

# fetch <path> <dest>: the text of <path> in the deployment repository on its default branch, into
# <dest>; on failure sets $fetch_error to what could not be done and GitHub's or base64's first
# line, and returns 1.
fetch() {
  local encoded
  fetch_error=""
  if ! encoded=$(gh api "repos/${deployment}/contents/$1" --jq .content 2>"$work/err"); then
    fetch_error="cannot read ${deployment} $1: $(first_line "$work/err")"
    return 1
  fi
  if ! printf '%s' "$encoded" | base64 -d >"$2" 2>"$work/err"; then
    fetch_error="cannot decode ${deployment} $1: $(first_line "$work/err")"
    return 1
  fi
}

# stack_file <path>: fetch <path> into $work/stack.yaml. An unreadable file is UNKNOWN, named with
# GitHub's answer.
stack_file() {
  if ! fetch "$1" "$work/stack.yaml"; then
    printf 'UNKNOWN: %s\n' "$fetch_error"
    return 1
  fi
}

# project_of <stack>: sets $project to the name <stack>/Pulumi.yaml declares, the Pulumi project
# whose own config keys <stack>/Pulumi.prod.yaml holds under `<name>:`. A file that cannot be read,
# or that declares no name, sets $project_error naming it and returns 1: the gate guesses no
# project's name, since a key under a guessed one would be read as the stack's own.
project_of() {
  project=""
  project_error=""
  if ! fetch "$1/Pulumi.yaml" "$work/project.yaml"; then
    project_error=$fetch_error
    return 1
  fi
  if ! project=$(python3 - "$work/project.yaml" 2>"$work/err" <<'PY'
import sys

import yaml

try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        document = yaml.safe_load(handle)
except UnicodeDecodeError:
    sys.exit("it is not UTF-8 text")
except yaml.YAMLError as error:
    sys.exit(f"it is not YAML: {str(error).splitlines()[0]}")
name = document.get("name") if isinstance(document, dict) else None
if isinstance(name, str) and name.strip():
    print(name.strip())
PY
  ); then
    project_error="cannot read ${deployment} $1/Pulumi.yaml: $(first_line "$work/err")"
    return 1
  fi
  if [ -z "$project" ]; then
    project_error="${deployment} $1/Pulumi.yaml declares no project name, so no key of $1/Pulumi.prod.yaml is known to be its own"
    return 1
  fi
}

# entries_state is unread, read or failed; load_entries reads the deployment's Legion projects once.
entries_state=unread
entries_error=""

# load_entries writes into $work/entries.json every Legion project the deployment declares, as one
# JSON object: "entries", the projects; "found", whether any are declared; and "missing", where it
# looked when none are. It reads what it needs itself, once for the whole run: legion/Pulumi.yaml's
# project name, and legion/Pulumi.prod.yaml ($work/legion.yaml). A deployment with legion/projects/
# declares its projects there alone, one mapping of a project's settings a file, read in file-name
# order, as the deployment's legion program reads them. Only a deployment with no such directory
# still declares them in the list under <project>:projects of legion/Pulumi.prod.yaml, as that
# program did before the files. A Pulumi.yaml or stack file it cannot read, a listing that fails
# other than as a missing directory, a file it cannot read, or one that is not one mapping leaves
# entries_state failed, with entries_error naming it.
load_entries() {
  [ "$entries_state" = unread ] || return 0
  local names name directory=1 legion_project
  entries_state=failed
  if ! project_of legion; then
    entries_error=$project_error
    return 0
  fi
  legion_project=$project
  if ! fetch legion/Pulumi.prod.yaml "$work/legion.yaml"; then
    entries_error=$fetch_error
    return 0
  fi
  entries_state=unread
  mkdir -p "$work/projects"
  if ! names=$(gh api "repos/${deployment}/contents/legion/projects" --jq '.[] | select(.type == "file") | .name' 2>"$work/err"); then
    if ! grep -qF 'HTTP 404' "$work/err"; then
      entries_state=failed
      entries_error="cannot list ${deployment} legion/projects: $(first_line "$work/err")"
      return 0
    fi
    names=""
    directory=0
  fi
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    if ! fetch "legion/projects/$name" "$work/projects/$name"; then
      entries_state=failed
      entries_error=$fetch_error
      return 0
    fi
  done <<<"$names"
  if ! python3 - "$work/legion.yaml" "${legion_project}:projects" "$work/projects" "$directory" >"$work/entries.json" 2>"$work/err" <<'PY'; then
import json
import os
import sys

import yaml

legion, key, directory, has_directory = sys.argv[1:5]
entries = []
if has_directory == "1":
    for name in sorted(os.listdir(directory)):
        try:
            with open(os.path.join(directory, name), encoding="utf-8") as handle:
                documents = list(yaml.safe_load_all(handle))
        except UnicodeDecodeError:
            sys.exit(f"legion/projects/{name} is not UTF-8 text")
        except yaml.YAMLError as error:
            sys.exit(f"legion/projects/{name} is not YAML: {str(error).splitlines()[0]}")
        if len(documents) != 1 or not isinstance(documents[0], dict):
            sys.exit(f"legion/projects/{name} is not one mapping of a project's settings")
        entries.append(documents[0])
    found, missing = bool(entries), "legion/projects/ holds no file"
else:
    listed = None
    if os.path.exists(legion):
        try:
            with open(legion, encoding="utf-8") as handle:
                document = yaml.safe_load(handle) or {}
        except UnicodeDecodeError:
            sys.exit("legion/Pulumi.prod.yaml is not UTF-8 text")
        except yaml.YAMLError as error:
            sys.exit(f"legion/Pulumi.prod.yaml is not YAML: {str(error).splitlines()[0]}")
        if not isinstance(document, dict) or not isinstance(document.get("config") or {}, dict):
            sys.exit("legion/Pulumi.prod.yaml is not a mapping with a config mapping")
        listed = (document.get("config") or {}).get(key)
    entries = listed if isinstance(listed, list) else []
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict):
            sys.exit(f"legion/Pulumi.prod.yaml's {key} list holds an entry that is not a mapping (entry {index + 1})")
    found = bool(entries)
    if isinstance(listed, list):
        missing = f"legion/projects/ does not exist, and legion/Pulumi.prod.yaml's {key} list is empty"
    else:
        missing = f"legion/projects/ does not exist, and legion/Pulumi.prod.yaml holds no {key} list"
# default=str writes a value YAML reads and JSON has no type for (a date) as its text.
json.dump({"entries": entries, "found": found, "missing": missing}, sys.stdout, default=str)
PY
    entries_state=failed
    entries_error="${deployment} $(first_line "$work/err")"
    return 0
  fi
  entries_state="read"
}

# daemon_pins: one line per project of $work/entries.json, tab-separated: the project's key, then
# READ and the image that runs its daemon, SKIP and why it runs none, HOST for a project whose
# daemon runs on a tmux host, RUNTIME and a runtime the gate does not know, or NOIMAGE. When the
# deployment declares no project it prints ERROR and where it looked.
daemon_pins() {
  python3 - "$work/entries.json" <<'PY'
import json
import sys

with open(sys.argv[1]) as handle:
    loaded = json.load(handle)
if not loaded["found"]:
    print(f"ERROR\tdeclares no project: {loaded['missing']}")
    sys.exit(0)
for entry in loaded["entries"]:
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

try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        document = yaml.safe_load(handle) or {}
except UnicodeDecodeError:
    sys.exit("it is not UTF-8 text")
except yaml.YAMLError as error:
    sys.exit(f"it is not YAML: {str(error).splitlines()[0]}")
if not isinstance(document, dict):
    sys.exit("it is not a mapping")
config = document.get("config") or {}
if not isinstance(config, dict):
    sys.exit("its config is not a mapping")
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
# rename makes one repository with cybermonklabs/legion. The record is the deployment's own: the
# repositoryRenames pairs of its Legion projects (load_entries), which render as each daemon's
# `repository_renames` and which the daemon's ghrepo.Renames reads
# (packages/daemon/internal/ghrepo/renames.go). They are the one place the transfer of
# sjawhar/legion to cybermonklabs/legion is recorded, so the gate keeps no copy of the pair.
# Both modes read the projects through load_entries, which reads each file once for the run. A
# record it cannot read, and a deployment that declares no project, leave aliases_state failed with
# the reason in aliases_error: an empty record would shut a build of this repository as built
# elsewhere.
load_aliases() {
  [ "$aliases_state" = unread ] || return 0
  load_entries
  if [ "$entries_state" = failed ]; then
    aliases_state=failed
    aliases_error=$entries_error
    return 0
  fi
  if ! python3 - "$work/entries.json" >"$work/aliases" 2>"$work/err" <<'PY'; then
import json
import sys

with open(sys.argv[1]) as handle:
    loaded = json.load(handle)
if not loaded["found"]:
    sys.exit(f"declares no project: {loaded['missing']}")
projects = loaded["entries"]
home = "cybermonklabs/legion"
for entry in projects:
    for pair in entry.get("repositoryRenames") or []:
        names = {str(pair.get("from", "")).lower(), str(pair.get("to", "")).lower()}
        if home in names:
            for name in names - {home, ""}:
                print(name)
PY
    aliases_state=failed
    aliases_error="${deployment} $(first_line "$work/err")"
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
      printf "UNKNOWN: %s: built from %s, and the repository renames of %s's Legion projects could not be read: %s\n" "$key" "$source" "$deployment" "$aliases_error"
      unknown=$((unknown + 1))
      return
    fi
    if ! grep -qxF -- "$repo" "$work/aliases"; then
      printf 'GATE SHUT: %s runs an image built from %s\n' "$key" "$source"
      shut=$((shut + 1))
      return
    fi
    via="; built as $source, which the repositoryRenames of the deployment's Legion projects make one repository with $legion_source"
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

# listener <key> <stack> <setting>: judges the listener pin of <stack> (envoy, envoy-cluster or
# dispatch), the value its Pulumi.prod.yaml sets for <setting> under the project name its
# Pulumi.yaml declares (`<project>:<setting>`).
listener() {
  local key=$1 stack=$2 setting=$3 ref
  if ! project_of "$stack"; then
    printf 'UNKNOWN: %s: %s\n' "$key" "$project_error"
    unknown=$((unknown + 1))
    return
  fi
  setting="${project}:${setting}"
  if ! stack_file "$stack/Pulumi.prod.yaml"; then
    unknown=$((unknown + 1))
    return
  fi
  if ! ref=$(config_value "$setting" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read %s/Pulumi.prod.yaml: %s\n' "$key" "$stack" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ -z "$ref" ]; then
    printf 'UNKNOWN: %s: %s/Pulumi.prod.yaml sets no %s\n' "$key" "$stack" "$setting"
    unknown=$((unknown + 1))
    return
  fi
  judge "$key" "$ref"
}

if [ "$mode" = listeners ]; then
  listener envoy envoy listenerImage
  listener envoy-cluster envoy-cluster envoyImage
  # dispatch's listener runs listenerImage and nothing else: the deployment's dispatch/main.go requires
  # that key (loadSettings) and pinListenerImage sets the listener's container from it, leaving
  # envoyImage to the Dispatch server. A stack that sets none runs no listener this gate can read,
  # so it is UNKNOWN rather than a reading of the server's image.
  listener dispatch dispatch listenerImage
else
  load_entries
  if [ "$entries_state" = failed ]; then
    printf 'UNKNOWN: %s\n' "$entries_error"
    unknown=$((unknown + 1))
    : >"$work/pins"
  elif ! daemon_pins >"$work/pins" 2>"$work/err"; then
    printf 'UNKNOWN: cannot read %s legion projects: %s\n' "$deployment" "$(first_line "$work/err")"
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
        printf 'UNKNOWN: %s %s\n' "$deployment" "$what"
        unknown=$((unknown + 1))
        ;;
    esac
  done <"$work/pins"
  if [ "$compared" -eq 0 ] && [ "$unknown" -eq 0 ]; then
    echo "UNKNOWN: the deployment's Legion projects run no daemon, so no pin was read"
    unknown=1
  fi
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
