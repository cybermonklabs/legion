# shellcheck shell=bash
# packages/envoy/scripts/lib/legion-deployment-projects.sh
#
# The reader of a deployment's Legion projects that daemon-release-gate.sh and
# daemon-running-gate.sh share, so the two never disagree on which daemons run. Each gate sources
# this file after it has set $deployment (the deployment repository, <owner>/<repo>) and $work (its
# private scratch directory); every function reads the repository with gh and writes into $work.

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

