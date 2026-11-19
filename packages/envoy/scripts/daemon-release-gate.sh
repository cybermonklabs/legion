#!/usr/bin/env bash
# packages/envoy/scripts/daemon-release-gate.sh --deployment <owner/repo>
#   [--listeners | --clients [--dotfiles <dir>]] <commit>
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
# and never guesses: a stack whose Pulumi.yaml is missing, declares no name or declares one Pulumi
# refuses is UNKNOWN, by name.
#
# The deployment's Legion projects are the files of legion/projects/ (`<nn>-<KEY>.yaml`, one mapping
# of a project's settings each, read in file-name order), as the deployment's legion program reads
# them. Only a deployment with no such directory still declares them in legion/Pulumi.prod.yaml's
# projects list, as that program did before the files (load_entries). An entry of the directory that
# is no file, a file with no download URL (GitHub lists a submodule so), and a listing of 1,000
# entries or more (GitHub lists no more), are UNKNOWN; so is a file GitHub serves with no content
# (fetch), as it serves one over 1 MB.
#
# Without a mode it reads the image that runs each Legion daemon: for each project, its
# `daemonImage` when set, else its `image` (legion/README.md's `daemonImage` row). A project that runs
# no daemon (`replicas: 0`, or `daemon: false`) is printed SKIPPED, by name. An entry at `runtime:
# tmux` runs its daemon on the project's host VM from `host.legionWorker`'s legion, not from its
# `image` (deployment legion/host.go, checkHost), and the gate reads no image for it: it is UNKNOWN
# by name, and so is an entry on any runtime but `kubernetes`, the default. A project file whose
# name is not `<nn>-<KEY>.yaml` or whose `key` is not its name's KEY, a `key` that is no project key
# in the list, a `replicas` that is no count, a `daemon` that is no boolean and an image that is no
# plain reference are UNKNOWN by name, never a project skipped or an image cut short.
#
# With --listeners it reads the listener pins instead: envoy-cluster/Pulumi.prod.yaml `envoyImage`,
# dispatch/Pulumi.prod.yaml `listenerImage`, which that stack's program sets on its listener in place
# of `envoyImage`, the Dispatch server's (dispatch/main.go, pinListenerImage), and, when the
# deployment declares the VM hub, envoy/Pulumi.prod.yaml `listenerImage`, which only that hub's
# listener reads. A dispatch stack that sets no `listenerImage` is UNKNOWN: its deployment requires
# that key, and the listener runs no other image. Whether the deployment declares the VM hub,
# gcp:NatsHub, is decided from its program, never from the pin, since the hub's removal deletes the
# pin with it (vm_hub, lib/daemon-release-gate-read-vm-hub.sh): at the head of the deployment
# repository's default branch, fetched as one archive and held to the commit's tree, a Go file that
# is not a test and uses NewNatsHub, or registers the hub's type token itself, declares it.
# Declared, it prints `VM HUB: <file>:<line> at <sha> uses NewNatsHub` (or `registers
# envoy-infra:gcp:NatsHub`) and reads the pin, a stack that sets none staying UNKNOWN; used
# nowhere, every file read, it prints `NO VM HUB: envoy/ at <sha> declares no gcp:NatsHub` and
# reads no VM pin. What it cannot fetch or read is UNKNOWN, a file the archive leaves out or changed,
# a Git LFS pointer and a submodule among it, since the archive holds none of their code; so a
# missing pin never reads as a hub that is gone. A stack pin that is no plain image reference is
# UNKNOWN and never read.
#
# With --clients it reads the clients that deliver to a session, each of which must carry the
# client follow filter of <commit>, since an older one passes a frame in a form it does not match in
# silence (lib/daemon-release-gate-read-image-clients.sh and
# lib/daemon-release-gate-read-developer-box.sh):
#   - in each `image` a running project names (each read once however many projects name it), and
#     in the agent-session image the deployment's sessions/ stack pins (sessions/Pulumi.prod.yaml
#     `<project>:image`, under the name sessions/Pulumi.yaml declares; that image is built FROM the
#     worker image and links its /opt/legion/pi-envoy), the two clients the worker build puts at
#     /opt/legion, /opt/legion/pi-envoy and /opt/legion/claude-envoy
#     (packages/daemon/docker/worker.Dockerfile), each of which must be there, judged by the commit
#     that build ran at, which the image's own /opt/legion/bin/legion prints (`legion version`),
#     never by its manifests' versions. A sessions/ stack or pin it cannot read is UNKNOWN by name.
#     The pins are what the gate reads, not running pods: a session launched with an image of its
#     own (`--image`) runs clients outside them, and a pod keeps the image it started with until it
#     is rolled or repinned, so right after a pin moves, GATE OPEN speaks for the pins while running
#     sessions may still load the older pi-envoy. An image not pinned by digest is UNKNOWN and never
#     run: under a tag, docker runs whatever image it holds by that name.
#   - for each running project's daemon, its `daemonImage`, else its `image`: no session loads the
#     clients of a daemon's own container, but the daemon installs `@sjawhar/pi-envoy@<version>`
#     into a kept session pod on a stale-plugin relaunch (packages/daemon/internal/daemon/
#     kubernetes.go, ResolvedPlugins). That version is the project's runtime.kubernetes.plugins
#     when its legion.yaml sets one, else the pair linked into the image's legion, its `legion
#     version` second line (`plugins @sjawhar/pi-legion <v> @sjawhar/pi-envoy <v>`), and it is
#     judged by the commit of its release tag, never by the image's build commit. The gate reads a
#     project's legion.yaml from the deployment's rendering of it,
#     legion/testdata/<key in lower case>-legion.yaml: the deployment's TestCommittedStackConfig
#     holds that file byte for byte to what its legion program renders (renderLegionYAML), and the
#     project files themselves carry no legion.yaml key. That program renders no plugins key today
#     (renderLegionYAML, legion/main.go:2743-2892 when this was written) and refuses a project field
#     it does not know (parseProjects' DisallowUnknownFields, legion/main.go:4771-4774), so the
#     linked pair decides; reading the rendering keeps the gate right if the program starts to set
#     the key. A rendering it cannot read or that sets no runtime.kubernetes, a legion that links no
#     pair where the rendering sets none, and a second line that names no pi-envoy version are
#     UNKNOWN. A project with no daemonImage runs its daemon from its image, which is then judged
#     both ways.
#   - A project that runs no daemon is SKIPPED, and one on a tmux host is UNKNOWN: its sessions run
#     the host's clients. Each command runs in the image with `docker run --rm --quiet --network
#     none --read-only --cap-drop ALL --security-opt no-new-privileges`, and every text read from
#     an image is printed with its control characters written as \x<hh>, so no image can print a
#     line of the gate's own.
#   - on this developer box, under --dotfiles (default ~/.dotfiles): the `@sjawhar/pi-envoy` pin of
#     omp/plugins/package.json and the copy installed beside it; each claude-envoy@legion-plugins
#     install the dotfiles' Claude Code plugin record holds, by its manifest; and the claude-envoy
#     version the marketplace legion-plugins serves to .claude/settings.json, read as Claude Code
#     reads it: its repo at its ref, the file at its path (one inside the repository, with no `.`
#     or `..` segment), and the package from its registry, which must be an https URL; a key of
#     either source this gate does not read is UNKNOWN. The version is the one npm's own pick takes
#     on this box, `npm pack <package>@<range> --dry-run --json` in a directory with no project
#     configuration, so npm applies every rule of its pick under the box's npm configuration (its
#     `tag`, `before`, `min-release-age`, deprecation and engines) and the gate judges that one
#     version. Each version is judged by the commit of its release tag (`pi-envoy-v<version>`,
#     `claude-envoy-v<version>`). A client installed where the dotfiles' pins say nothing about it
#     (~/.omp/plugins or a profile's plugin tree, ~/.claude, another CLAUDE_CONFIG_DIR), a link that
#     does not resolve, and a marketplace entry it cannot read are UNKNOWN.
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
# It prints one line per pin or surface: `PASS`, `GATE SHUT` (behind <commit>, or built elsewhere),
# `UNKNOWN` (a label, a file, a package, a version or a comparison it could not read, a revision
# that is no commit id, or a reader it does not read), `SKIPPED`, or `VM HUB` or `NO VM HUB`; then
# the verdict, alone on the last line: `GATE OPEN` (exit 0) only when every pin it read passed and it
# read at least one, `GATE SHUT` (exit 1) when any pin shut it, and `UNKNOWN` (exit 3) otherwise.
# Every text it reads from outside it is printed with its control characters written as \x<hh>
# (printable), so none can start or rewrite a line of the gate's own. A usage error, a missing
# --deployment, both modes, or a --dotfiles without --clients among them, exits 2 and reads nothing.
#
# It needs bash, gh (signed in with read access to the deployment repository and cybermonklabs/legion),
# docker with buildx, jq, base64, python3 with PyYAML and, for --clients, npm. Docker must be able to
# read each pin's registry (and, for --clients, to pull and run each image), with logins made into a
# config directory of your own so the shared one is untouched. For ghcr.io, a token holding
# read:packages; for the Artifact Registry images (`<region>-docker.pkg.dev`), a Google login that
# may read them, in the same DOCKER_CONFIG. For example:
#   export DOCKER_CONFIG=$(mktemp -d)
#   gh auth token | docker login ghcr.io -u x --password-stdin
#   gcloud auth print-access-token | docker login <region>-docker.pkg.dev -u oauth2accesstoken --password-stdin
# Any read-only token holding read:packages serves where `gh auth token` is not allowed, and
# `gcloud auth configure-docker <region>-docker.pkg.dev`, run with that DOCKER_CONFIG set, makes
# docker ask docker-credential-gcloud instead of keeping a token that expires within the hour.
# With only the ghcr.io login, every Artifact Registry image prints UNKNOWN.
# A pin docker cannot read prints UNKNOWN with docker's own answer, never GATE OPEN.
set -euo pipefail

usage() {
  [ -z "${1:-}" ] || echo "daemon-release-gate.sh: $1" >&2
  echo "usage: daemon-release-gate.sh --deployment <owner/repo> [--listeners | --clients [--dotfiles <dir>]] <commit>" >&2
  echo "  <owner/repo> is the deployment repository whose Pulumi stacks pin the readers' images" >&2
  echo "  <commit> is the cybermonklabs/legion commit, 7 to 40 hex digits, every reader must run at or after" >&2
  echo "  <dir> is the developer box's dotfiles, whose client pins --clients reads (default ~/.dotfiles)" >&2
  exit 2
}

mode=daemons
deployment=""
dotfiles=${HOME:+${HOME}/.dotfiles}
dotfiles_given=0
listeners=0
clients=0
while [ "$#" -gt 1 ]; do
  case "$1" in
    --listeners) listeners=1; shift ;;
    --clients) clients=1; shift ;;
    --deployment) deployment=$2; shift 2 ;;
    --deployment=*) deployment=${1#--deployment=}; shift ;;
    --dotfiles) dotfiles=$2; dotfiles_given=1; shift 2 ;;
    --dotfiles=*) dotfiles=${1#--dotfiles=}; dotfiles_given=1; shift ;;
    *) break ;;
  esac
done
# A `--deployment` with its value left out takes the commit as the deployment and leaves no commit.
[ "$#" -gt 0 ] || usage "no <commit> follows the flags${deployment:+; --deployment took ${deployment} as its <owner/repo>}"
[ "$#" -eq 1 ] && [[ $1 =~ ^[0-9a-f]{7,40}$ ]] || usage
[ -n "$deployment" ] || usage "--deployment <owner/repo> is required: the deployment repository whose stacks the gate reads"
[[ $deployment =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage "--deployment ${deployment} is not <owner>/<repo>"
[ "$listeners$clients" != 11 ] || usage "--listeners and --clients read different readers; give one"
# A flag the run would not read is refused, never ignored: an operator who gave one meant it read.
[ "$dotfiles_given" = 0 ] || [ "$clients" = 1 ] || usage "--dotfiles is read only by --clients"
[ "$listeners" = 0 ] || mode=listeners
[ "$clients" = 0 ] || mode=clients
readonly commit=$1 deployment dotfiles mode
readonly legion_source="https://github.com/cybermonklabs/legion"
# A plain image reference: nothing in it a shell, docker's own options or a tab-separated line of
# the gate's could read as anything else.
readonly image_reference='^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$'

work=$(mktemp -d)
readonly work
trap 'rm -rf "$work"' EXIT

shut=0
unknown=0
compared=0

# printable <text>: <text> with each character that is not printable, a newline, a carriage return,
# a tab and an escape among them, written as \x<hh> (\u<hhhh> or \U<hhhhhhhh> past U+00FF), so text
# the gate reads from outside it (an image, an archive, a marketplace, the dotfiles, an error a
# command printed) can neither start a line of the gate's output nor rewrite one on a terminal.
printable() {
  python3 -c '
import sys

def shown(c):
    if c.isprintable():
        return c
    n = ord(c)
    return "\\x%02x" % n if n < 0x100 else "\\u%04x" % n if n < 0x10000 else "\\U%08x" % n

print("".join(shown(c) for c in sys.argv[1]))
' "$1"
}

# first_line <file> and last_line <file>: that line of an error a command left, printable.
first_line() { printable "$(head -n 1 "$1")"; }
last_line() { printable "$(tail -n 1 "$1")"; }

# fetch <path> <dest>: the text of <path> in the deployment repository on its default branch, into
# <dest>; on failure sets $fetch_error to what could not be done and GitHub's or base64's first
# line, and returns 1. GitHub serves a file of 1 to 100 MB with encoding `none` and no content, so
# any encoding but base64 is a file the gate could not read, never an empty one.
fetch() {
  local answer encoding content=""
  fetch_error=""
  if ! answer=$(gh api "repos/${deployment}/contents/$1" --jq '.encoding, .content' 2>"$work/err"); then
    fetch_error="cannot read ${deployment} $(printable "$1"): $(first_line "$work/err")"
    return 1
  fi
  encoding=${answer%%$'\n'*}
  [[ $answer != *$'\n'* ]] || content=${answer#*$'\n'}
  if [ "$encoding" != base64 ]; then
    fetch_error="cannot read ${deployment} $(printable "$1"): GitHub serves it with encoding $(printable "$encoding") and no content, as it serves a file over 1 MB"
    return 1
  fi
  if ! printf '%s' "$content" | base64 -d >"$2" 2>"$work/err"; then
    fetch_error="cannot decode ${deployment} $(printable "$1"): $(first_line "$work/err")"
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
# that declares no name, or whose name Pulumi would refuse (letters, digits, `_`, `.` and `-`, 1 to
# 100 of them: ValidateProjectName, sdk/go/common/tokens/project.go) sets $project_error naming it
# and returns 1: the gate guesses no project's name, since a key under a guessed one would be read
# as the stack's own.
project_of() {
  project=""
  project_error=""
  if ! fetch "$1/Pulumi.yaml" "$work/project.yaml"; then
    project_error=$fetch_error
    return 1
  fi
  if ! project=$(python3 - "$work/project.yaml" 2>"$work/err" <<'PY'
import json
import re
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
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,100}", name.strip()):
        sys.exit(f"its name {json.dumps(name.strip())} is no Pulumi project name")
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
# other than as a missing directory, a file it cannot read, one that is not one mapping, and one
# that program refuses (projectFileRule: a name that is not `<nn>-<KEY>.yaml`, or a `key` that is not
# that KEY) leave entries_state failed, with entries_error naming it.
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
  # GitHub's listing names each entry with its type; the deployment's program reads every entry of
  # the directory, so one that is no file (a link, a directory) is not skipped but refused, and so is
  # a listing that may have stopped at the 1,000 entries GitHub lists at most. GitHub lists a
  # submodule as a file with no download URL, so that is refused too.
  if ! names=$(gh api "repos/${deployment}/contents/legion/projects" --jq '.[] | [.type, (if .download_url == null then "none" else "url" end), .name] | @tsv' 2>"$work/err"); then
    if ! grep -qF 'HTTP 404' "$work/err"; then
      entries_state=failed
      entries_error="cannot list ${deployment} legion/projects: $(first_line "$work/err")"
      return 0
    fi
    names=""
    directory=0
  fi
  if [ "$(grep -c . <<<"$names")" -ge 1000 ]; then
    entries_state=failed
    entries_error="${deployment} legion/projects/ holds 1000 entries or more, and GitHub lists at most 1,000, so the gate cannot read every project"
    return 0
  fi
  while IFS=$'\t' read -r type url name; do
    [ -n "$name" ] || continue
    if [ "$type" != file ]; then
      entries_state=failed
      entries_error="${deployment} legion/projects/$(printable "$name") is a $(printable "$type"), which this gate does not read as a project file"
      return 0
    fi
    if [ "$url" = none ]; then
      entries_state=failed
      entries_error="${deployment} legion/projects/$(printable "$name") is listed as a file with no download URL, as GitHub lists a submodule, which this gate does not read as a project file"
      return 0
    fi
    if ! fetch "legion/projects/$name" "$work/projects/$name"; then
      entries_state=failed
      entries_error=$fetch_error
      return 0
    fi
  done <<<"$names"
  if ! python3 - "$work/legion.yaml" "${legion_project}:projects" "$work/projects" "$directory" >"$work/entries.json" 2>"$work/err" <<'PY'; then
import json
import os
import re
import sys

import yaml

legion, key, directory, has_directory = sys.argv[1:5]
entries = []
if has_directory == "1":
    # The deployment's legion program refuses a file it cannot name a project by: its name must be
    # <nn>-<KEY>.yaml and its key that KEY (projectFileRule), so the gate takes neither as a project.
    rule = re.compile(r"[0-9]{2}-([A-Z][A-Z0-9]*)\.yaml")
    for name in sorted(os.listdir(directory)):
        named = rule.fullmatch(name)
        if not named:
            sys.exit(f"legion/projects/{name} is not named <nn>-<KEY>.yaml, so no key is known to be its own")
        try:
            with open(os.path.join(directory, name), encoding="utf-8") as handle:
                documents = list(yaml.safe_load_all(handle))
        except UnicodeDecodeError:
            sys.exit(f"legion/projects/{name} is not UTF-8 text")
        except yaml.YAMLError as error:
            sys.exit(f"legion/projects/{name} is not YAML: {str(error).splitlines()[0]}")
        if len(documents) != 1 or not isinstance(documents[0], dict):
            sys.exit(f"legion/projects/{name} is not one mapping of a project's settings")
        if documents[0].get("key") != named.group(1):
            sys.exit(f"legion/projects/{name} declares key {documents[0].get('key')!r}, not {named.group(1)}")
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

# daemon_pins <daemons|clients>: one line per project of $work/entries.json, tab-separated: the
# project's key, then READ and the image that runs its daemon, SKIP and why it runs none, HOST for a
# project whose daemon runs on a tmux host, RUNTIME and a runtime the gate does not know, NOIMAGE,
# or INVALID and the field it cannot take. With clients, a running project's lines are keyed by the
# field they read, `<key> daemonImage` or `<key> image`: READ and its image, whose clients a session
# pod loads; and DAEMON, the image that runs its daemon (its daemonImage, else its image), and the
# project's rendered legion.yaml, legion/testdata/<key in lower case>-legion.yaml, since what that
# daemon gives a session is the pi-envoy it installs on a relaunch. A key that is no project key
# ([A-Z][A-Z0-9]*, the rule legion/projects/ holds its file names to) is INVALID in either shape:
# it names no golden, and a tab or a newline in it would cut this line, so its text could set the
# verdict. A field of the wrong type (a replicas that is no count, as YAML's false would equal 0; a
# daemon that is no boolean) or an image that is no plain reference (a tab in it would cut this
# line, and the gate would read another image) is INVALID, never a project skipped or an image cut
# short. When the deployment declares no project it prints ERROR and where it looked.
daemon_pins() {
  python3 - "$work/entries.json" "$1" "$image_reference" <<'PY'
import json
import re
import sys

with open(sys.argv[1]) as handle:
    loaded = json.load(handle)
clients = sys.argv[2] == "clients"
reference = re.compile(sys.argv[3])
if not loaded["found"]:
    print(f"ERROR\tdeclares no project: {loaded['missing']}")
    sys.exit(0)
for entry in loaded["entries"]:
    named = entry.get("key")
    if not (isinstance(named, str) and re.fullmatch(r"[A-Z][A-Z0-9]*", named)):
        label = "<an entry with no key>" if named is None else "<an entry>"
        golden_note = ", so no legion/testdata golden is its own" if clients else ""
        print(f"{label}\tINVALID\tkey {json.dumps(named)}, which is no project key{golden_note}")
        continue
    key = named
    replicas, daemon, runtime = entry.get("replicas"), entry.get("daemon"), entry.get("runtime")
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
    elif runtime == "tmux":
        print(f"{key}\tHOST")
    elif runtime not in (None, "", "kubernetes"):
        print(f"{key}\tRUNTIME\t{runtime if isinstance(runtime, str) and runtime.isprintable() else json.dumps(runtime)}")
    elif bad:
        print(f"{key}\tINVALID\t{bad[0]} {json.dumps(images[bad[0]])}, which is no image reference")
    elif not images:
        print(f"{key}\tNOIMAGE")
    elif clients:
        golden = f"legion/testdata/{key.lower()}-legion.yaml"
        if "daemonImage" in images:
            print(f"{key} daemonImage\tDAEMON\t{images['daemonImage']}\t{golden}")
        if "image" in images:
            print(f"{key} image\tREAD\t{images['image']}")
            if "daemonImage" not in images:
                print(f"{key} image\tDAEMON\t{images['image']}\t{golden}")
    else:
        print(f"{key}\tREAD\t{images.get('daemonImage') or images['image']}")
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
          printf 'UNKNOWN: %s: cannot read the %s image of %s: %s\n' "$key" "$(printable "$platform")" "$ref" "$(first_line "$work/err")"
          return 1
        fi
        if ! labels_of "$labels"; then
          printf 'UNKNOWN: %s: the %s labels of %s are not JSON: %s\n' "$key" "$(printable "$platform")" "$ref" "$(first_line "$work/err")"
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
        printf 'UNKNOWN: %s platforms disagree: %s\n' "$key" "$(printable "$answers")"
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
      printf 'UNKNOWN: %s: %s has media type %s, which is neither an image nor an index\n' "$key" "$ref" "$(printable "$mediatype")"
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
  local key=$1 ref=$2 repo via=""
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
      printf "UNKNOWN: %s: built from %s, and the repository renames of %s's Legion projects could not be read: %s\n" "$key" "$(printable "$source")" "$deployment" "$aliases_error"
      unknown=$((unknown + 1))
      return
    fi
    if ! grep -qxF -- "$repo" "$work/aliases"; then
      printf 'GATE SHUT: %s runs an image built from %s\n' "$key" "$(printable "$source")"
      shut=$((shut + 1))
      return
    fi
    via="; built as $(printable "$source"), which the repositoryRenames of the deployment's Legion projects make one repository with $legion_source"
  fi
  if ! [[ $revision =~ ^[0-9a-f]{7,40}$ ]]; then
    printf 'UNKNOWN: %s: %s names revision %s, which is no commit id\n' "$key" "$ref" "$(printable "$revision")"
    unknown=$((unknown + 1))
    return
  fi
  compare_revision "$key" "$revision" "$revision" "$via"
}

# compare_revision <key> <what it runs> <revision> <note>: compares <revision>, a commit id, with
# <commit> in cybermonklabs/legion and prints <key>'s line, saying it runs <what it runs> and ending
# in <note>: PASS when GitHub answers `ahead` or `identical` with `behind_by` 0, GATE SHUT when it
# is behind or diverged, UNKNOWN otherwise.
compare_revision() {
  local key=$1 runs=$2 revision=$3 via=$4 answer status behind_by
  if ! answer=$(gh api "repos/cybermonklabs/legion/compare/${commit}...${revision}" --jq '{status,behind_by}' 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot compare %s...%s in cybermonklabs/legion: %s\n' "$key" "$commit" "$revision" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! IFS=$'\t' read -r status behind_by < <(jq -r '[.status, .behind_by] | @tsv' <<<"$answer" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read GitHub'\''s comparison of %s...%s: %s\n' "$key" "$commit" "$revision" "$(printable "$answer")"
    unknown=$((unknown + 1))
    return
  fi
  case "$status:$behind_by" in
    ahead:0) printf 'PASS: %s runs %s (ahead of %s)%s\n' "$key" "$runs" "$commit" "$via" ;;
    identical:0) printf 'PASS: %s runs %s (identical to %s)%s\n' "$key" "$runs" "$commit" "$via" ;;
    behind:*)
      printf 'GATE SHUT: %s runs %s (behind %s by %s)%s\n' "$key" "$runs" "$commit" "$(printable "$behind_by")" "$via"
      shut=$((shut + 1))
      ;;
    diverged:*)
      printf 'GATE SHUT: %s runs %s (diverged from %s, behind by %s)%s\n' "$key" "$runs" "$commit" "$(printable "$behind_by")" "$via"
      shut=$((shut + 1))
      ;;
    *)
      printf 'UNKNOWN: %s: GitHub compared %s...%s as %s, behind by %s\n' "$key" "$commit" "$revision" "$(printable "$status")" "$(printable "$behind_by")"
      unknown=$((unknown + 1))
      ;;
  esac
}

# stack_pin <key> <stack> <setting>: sets $pin to the value <stack>/Pulumi.prod.yaml sets for
# <setting> under the project name its Pulumi.yaml declares (`<project>:<setting>`, $project the
# name) and returns 0; or prints <key>'s UNKNOWN line naming what it could not read, counts it, and
# returns 1. A pin that is no plain image reference (a newline in it would start a line of the
# gate's, and an option would reach docker) is UNKNOWN, never read.
stack_pin() {
  local key=$1 stack=$2 setting=$3
  pin=""
  if ! project_of "$stack"; then
    printf 'UNKNOWN: %s: %s\n' "$key" "$project_error"
    unknown=$((unknown + 1))
    return 1
  fi
  setting="${project}:${setting}"
  if ! stack_file "$stack/Pulumi.prod.yaml"; then
    unknown=$((unknown + 1))
    return 1
  fi
  if ! pin=$(config_value "$setting" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read %s/Pulumi.prod.yaml: %s\n' "$key" "$stack" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return 1
  fi
  if [ -z "$pin" ]; then
    printf 'UNKNOWN: %s: %s/Pulumi.prod.yaml sets no %s\n' "$key" "$stack" "$setting"
    unknown=$((unknown + 1))
    return 1
  fi
  if ! [[ $pin =~ $image_reference ]]; then
    printf 'UNKNOWN: %s: %s/Pulumi.prod.yaml sets %s to %s, which is no image reference\n' "$key" "$stack" "$setting" "$(printable "$pin")"
    unknown=$((unknown + 1))
    return 1
  fi
}

# listener <key> <stack> <setting>: judges the listener pin of <stack> (envoy, envoy-cluster or
# dispatch), the value its Pulumi.prod.yaml sets for <setting> (stack_pin).
listener() {
  if stack_pin "$@"; then
    judge "$1" "$pin"
  fi
}

# The readers in lib/: the VM hub, which --listeners decides before it reads envoy's pin, and the
# clients --clients reads in each worker image and on the developer box.
gate_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib"
readonly gate_lib
# shellcheck source=lib/daemon-release-gate-read-vm-hub.sh
source "${gate_lib}/daemon-release-gate-read-vm-hub.sh"
# shellcheck source=lib/daemon-release-gate-read-image-clients.sh
source "${gate_lib}/daemon-release-gate-read-image-clients.sh"
# shellcheck source=lib/daemon-release-gate-read-developer-box.sh
source "${gate_lib}/daemon-release-gate-read-developer-box.sh"

if [ "$mode" = listeners ]; then
  # envoy's listenerImage is read only by the VM hub's listener, and the hub's removal deletes the
  # key with it, so the leg decides from the deployment's program whether the hub is declared
  # (vm_hub) before it reads that pin: a declared hub whose pin is gone stays UNKNOWN, and a program
  # that declares none, every file of it read, reads no VM pin.
  if ! vm_hub; then
    unknown=$((unknown + 1))
  elif [ "$hub" = yes ]; then
    printf 'VM HUB: %s at %s %s\n' "$hub_use" "$hub_head" "$hub_how"
    listener envoy envoy listenerImage
  else
    printf 'NO VM HUB: envoy/ at %s declares no gcp:NatsHub\n' "$hub_head"
  fi
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
  elif ! daemon_pins "$mode" >"$work/pins" 2>"$work/err"; then
    printf 'UNKNOWN: cannot read %s legion projects: %s\n' "$deployment" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
  fi
  while IFS=$'\t' read -r key what detail golden; do
    case "$what:$mode" in
      SKIP:*) printf 'SKIPPED: %s runs no daemon (%s)\n' "$key" "$detail" ;;
      READ:clients) client_image "$key" "$detail" ;;
      DAEMON:clients) client_daemon "$key" "$detail" "$golden" ;;
      READ:*) judge "$key" "$detail" ;;
      NOIMAGE:*)
        printf 'UNKNOWN: %s runs a daemon and names neither daemonImage nor image\n' "$key"
        unknown=$((unknown + 1))
        ;;
      HOST:clients)
        printf 'UNKNOWN: %s runs its sessions on a tmux host (runtime: tmux), whose clients this gate does not read\n' "$key"
        unknown=$((unknown + 1))
        ;;
      HOST:*)
        printf 'UNKNOWN: %s runs its daemon on a tmux host (runtime: tmux), from host.legionWorker, an image this gate does not read\n' "$key"
        unknown=$((unknown + 1))
        ;;
      RUNTIME:*)
        printf 'UNKNOWN: %s names runtime %s, which this gate does not know, so it reads no image for its daemon\n' "$key" "$detail"
        unknown=$((unknown + 1))
        ;;
      INVALID:*)
        printf 'UNKNOWN: %s names %s\n' "$key" "$detail"
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
  if [ "$mode" = clients ]; then
    # The agent sessions the deployment's sessions/ stack launches run the image it pins, and the
    # developer box delivers to its own sessions through the clients its dotfiles install.
    sessions_image
    dotfiles_clients
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
