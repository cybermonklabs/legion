# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-read-developer-box.sh
#
# The --clients reader of the developer box and of its versions, which daemon-release-gate.sh
# sources; it is not run on its own. It reads the gate's dotfiles (--dotfiles, default ~/.dotfiles),
# work, unknown, printable, first_line and last_line, and its compare_revision. Text it reads from
# the dotfiles, a marketplace or npm is printed through printable.
# shellcheck disable=SC2154 # dotfiles and work are the gate's

# release_commit <package> <version>: sets $tag to <package>-v<version>, the release tag of that
# version, and $tag_commit to the commit it names in cybermonklabs/legion; GitHub's error into
# $work/err.
release_commit() {
  tag="$1-v$2"
  mkdir -p "$work/tags"
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

# A release version, as Semantic Versioning writes one with no build metadata: no leading zero in a
# number, no empty identifier in a pre-release. It is the one grammar of a version here.
readonly release_version='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'

# The node program that prints npm-package-arg's type of the spec GATE_SPEC names (range, version,
# tag, directory, file, git, remote or alias), with the npm-package-arg npm ships, which resolves
# from npm's own npm-cli.js (npm_execpath); a spec it refuses prints its message and exits 1.
# shellcheck disable=SC2016 # JavaScript, kept as text
readonly spec_type_js='
const path = require("path");
const npa = require(require.resolve("npm-package-arg", { paths: [path.dirname(process.env.npm_execpath)] }));
try {
  process.stdout.write(npa(process.env.GATE_SPEC).type + "\n");
} catch (error) {
  process.stderr.write(String(error.message).split("\n")[0] + "\n");
  process.exit(1);
}'

# judge_release <label> <package> <version> [<note>]: compares the commit <package>'s release
# <version> was tagged at, and prints <label>'s line. A version that is not one exact release
# version (a pre-release one counts; a range or a build suffix does not), or one no release tag
# names, is UNKNOWN.
judge_release() {
  local label=$1 package=$2 version=$3 note=${4:-}
  if ! [[ $version =~ $release_version ]]; then
    printf 'UNKNOWN: %s: %s %s is no exact release version\n' "$label" "$package" "$(printable "$version")"
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

# installed_claude_envoy <installed_plugins.json> [<configuration>]: one `<plugin>\t<version>` line
# per install of a claude-envoy@<marketplace> the Claude Code plugin record holds, in either of its
# two shapes (a plugin's value an install, or a list of them). Given the Claude Code configuration
# the record is its own, each install's installPath must be a directory inside it holding the
# @sjawhar/claude-envoy manifest, and the version printed is that manifest's, the copy a session
# loads, not the record's word for it; the record's follows when the two differ. A record or an
# install that cannot be read as one fails, naming why.
installed_claude_envoy() {
  python3 - "$@" <<'PY'
import json
import os
import re
import sys

name_of = sys.argv[1]
config = os.path.realpath(sys.argv[2]) if len(sys.argv) > 2 else None
try:
    with open(name_of) as handle:
        record = json.load(handle)
except (OSError, ValueError) as error:
    sys.exit(f"{name_of} cannot be read as JSON: {error}")
if not isinstance(record, dict) or not isinstance(record.get("plugins"), dict):
    sys.exit(f"{name_of} holds no plugins object")
for name, installs in record["plugins"].items():
    if not name.startswith("claude-envoy@"):
        continue
    if not re.fullmatch(r"claude-envoy@[A-Za-z0-9][A-Za-z0-9._-]*", name):
        sys.exit(f"{name_of} records a plugin named {json.dumps(name)}, which is no plugin name this gate can read")
    installs = [installs] if isinstance(installs, dict) else installs
    if not isinstance(installs, list) or not installs:
        sys.exit(f"{name_of} records {name} as no install")
    for install in installs:
        version = install.get("version") if isinstance(install, dict) else None
        if not isinstance(version, str) or not version.isprintable():
            sys.exit(f"{name_of} records an install of {name} with no version")
        if config is None:
            print(f"{name}\t{version}")
            continue
        path = install.get("installPath")
        if not isinstance(path, str) or not path:
            sys.exit(f"{name_of} records an install of {name} with no installPath")
        real = os.path.realpath(path)
        if not real.startswith(config + os.sep):
            sys.exit(f"{name_of} records an install of {name} at {path}, outside {config}")
        try:
            with open(os.path.join(real, "package.json")) as handle:
                manifest = json.load(handle)
        except (OSError, ValueError) as error:
            sys.exit(f"{name_of} records an install of {name} at {path}, whose package.json cannot be read: {error}")
        installed = manifest.get("version") if isinstance(manifest, dict) else None
        if not isinstance(manifest, dict) or manifest.get("name") != "@sjawhar/claude-envoy" or not isinstance(installed, str) or not installed.isprintable():
            sys.exit(f"{name_of} records an install of {name} at {path}, which holds no @sjawhar/claude-envoy manifest with a version")
        print(f"{name}\t{installed}" + ("" if installed == version else f"\t{version}"))
PY
}

# The one python3 this read speaks to, in place of a start of its own for each path. The four
# readers below (entry_state, utf8, unresolved, plugin_trees) each ask the filesystem one question
# that bash cannot ask, and a --clients run asks about a dozen of them: a python3 start a question
# is most of what such a run costs in processes (287 for one --clients run of the gate's test, 17
# of them python3; a start is about 20 ms where the question itself is a stat).
#
# box_reader_start opens it, and every question goes over a pipe: a frame is the verb and its
# arguments, each ended by a NUL byte, and an empty one ends the frame; the answer comes back the
# same way, its first token saying what kind of answer it is. No argument is empty, and no path
# holds a NUL, so the framing carries a name of any bytes. The reader is opened before the first
# question, by the shell that reads the dotfiles, so every `$(...)` below inherits its pipes; it
# ends when the gate does and its pipes close. A question it does not answer is UNKNOWN by name,
# never a pass: the gate reads no silence as an answer.
#
# A path in an answer, a plugin tree of plugin_trees, goes back as its own bytes (os.fsencode),
# the bytes of the name on disk, whatever locale Python reads names in: under LC_ALL=C it reads
# café as caf\udcc3\udca9, and those characters written back as text would name a path that is not
# there, so the profile would read as empty. Text an answer explains (an entry_state reason, an
# unresolved one, why a listing failed) is written as text under PYTHONIOENCODING=:backslashreplace,
# the way Python writes an error, so a name in it that is no UTF-8 comes back with its bytes escaped
# rather than failing the write.
box_reader_started=0
box_reader_start() {
  [ "$box_reader_started" = 0 ] || return 0
  cat >"$work/box-reader.py" <<'PY'
import os
import stat
import sys

incoming = sys.stdin.buffer
outgoing = sys.stdout.buffer
encoding = sys.stdout.encoding
errors = sys.stdout.errors


def frame():
    """The next question's tokens as raw bytes, or None once the gate has closed the pipe."""
    tokens = []
    token = bytearray()
    while True:
        byte = incoming.read(1)
        if not byte:
            return None
        if byte != b"\0":
            token += byte
            continue
        if not token:
            return tokens
        tokens.append(bytes(token))
        token = bytearray()


def answer(*parts):
    """Each part is bytes, sent as they are, or text, sent in the locale's encoding with each
    character it cannot write escaped."""
    for part in parts:
        if isinstance(part, str):
            part = part.encode(encoding, errors)
        outgoing.write(part + b"\0")
    outgoing.write(b"\0")
    outgoing.flush()


def state(path):
    """present when something is at path, a link whose target is gone included; absent when nothing
    is and the directory that would hold it was searched; unknown and why otherwise."""
    try:
        os.lstat(path)
        return "present"
    except FileNotFoundError:
        pass
    except OSError as error:
        return f"unknown {path}: {error.strerror}"
    # Nothing is at path, and the deepest ancestor that is decides why: a link there whose target is
    # gone hides what would be beneath it.
    parent = path
    while True:
        parent = os.path.dirname(parent) or "."
        try:
            mode = os.lstat(parent).st_mode
        except FileNotFoundError:
            continue
        except OSError as error:
            return f"unknown {parent}: {error.strerror}"
        break
    # Its target, read from the link's own directory, is gone, or is there and does not resolve (a
    # link whose target is gone). Resolving the link fails with nothing there, as path's lookup did:
    # any other error, as at a directory the gate may not search or a loop, failed that lookup above
    # and is named.
    if stat.S_ISLNK(mode) and not os.path.exists(parent):
        target = os.readlink(parent)
        why = "resolve" if os.path.lexists(os.path.join(os.path.dirname(parent), target)) else "exist"
        return f"unknown {parent} is a link to {target}, which does not {why}"
    return "absent"


def unresolved(link):
    """How resolving link fails, as the words after "a link to <its target>, which", or None when
    it resolves."""
    try:
        os.stat(link)
    except FileNotFoundError:
        target = os.path.join(os.path.dirname(link), os.readlink(link))
        return "does not resolve" if os.path.lexists(target) else "does not exist"
    except OSError as error:
        return f"does not resolve: {error.strerror}"
    return None


def trees(home):
    """The Oh My Pi plugin trees of this box, ~/.omp/plugins and each profile's, and None; or, for a
    profiles directory it cannot list (a link there whose target is gone among them) and a profile
    whose name is no UTF-8, which the gate may not get unchanged, the trees it found before it
    stopped, ~/.omp/plugins, and why it stopped. The caller still reads ~/.omp/plugins after such
    a failure, so its line names every surface it could not see at once."""
    omp = os.path.join(home, ".omp")
    profiles = os.path.join(omp, "profiles")
    found = [os.path.join(omp, "plugins")]
    try:
        entries = sorted(os.listdir(profiles))
    except FileNotFoundError:
        # Nothing there is no profile, but a link there whose target is gone hides every profile it
        # would hold. A link above it whose target is gone is above ~/.omp/plugins too, and the
        # state of that path reports it there; a link that loops fails the listing, as any other
        # error does. The caller prints the last line of the failure, so a newline in the target is
        # written as printable does.
        if os.path.islink(profiles):
            target = os.readlink(profiles)
            why = "resolve" if os.path.lexists(os.path.join(omp, target)) else "exist"
            shown = target.replace("\n", "\\x0a")
            return found, f"{profiles} is a link to {shown}, which does not {why}"
        entries = []
    except OSError as error:
        return found, f"{profiles} cannot be listed: {error.strerror}"
    # The name's own bytes decide, whatever encoding this locale reads names with.
    for entry in entries:
        path = os.fsencode(os.path.join(profiles, entry))
        try:
            os.fsencode(entry).decode("utf-8")
        except UnicodeDecodeError:
            shown = path.decode("utf-8", "backslashreplace")
            return found, shown.replace("\n", "\\x0a") + ": the profile's name is no UTF-8"
    for entry in entries:
        found.append(os.path.join(profiles, entry, "plugins"))
    return found, None


while True:
    question = frame()
    if question is None:
        break
    verb = question[0].decode("utf-8", "replace")
    if verb == "state" and len(question) == 2:
        answer("ok", state(os.fsdecode(question[1])))
    elif verb == "utf8" and len(question) == 2:
        try:
            question[1].decode("utf-8")
        except UnicodeDecodeError:
            answer("no")
        else:
            answer("yes")
    elif verb == "unresolved" and len(question) == 2:
        why = unresolved(os.fsdecode(question[1]))
        answer("no") if why is None else answer("yes", why)
    elif verb == "trees" and len(question) == 2:
        found, problem = trees(os.fsdecode(question[1]))
        paths = [os.fsencode(tree) for tree in found]
        answer("ok", *paths) if problem is None else answer("fail", problem, *paths)
    else:
        answer("fail", f"the gate asked its path reader {verb}, which it does not answer")
PY
  # shellcheck disable=SC2034 # bash sets BOX_READER's two pipes and BOX_READER_PID
  coproc BOX_READER { PYTHONIOENCODING=:backslashreplace python3 "$work/box-reader.py"; }
  box_reader_started=1
}

# box_reader_call <verb> <argument>...: asks the reader, and sets box_answer to the tokens of its
# answer. It returns 1 when the reader did not answer, or is no longer there to ask (bash unsets
# BOX_READER once it reaps the reader), which each reader below turns into UNKNOWN.
box_answer=()
box_reader_call() {
  local token to=${BOX_READER[1]-} from=${BOX_READER[0]-}
  box_answer=()
  [ -n "$to" ] && [ -n "$from" ] || return 1
  printf '%s\0' "$@" '' >&"$to" || return 1
  while IFS= read -r -d '' token <&"$from"; do
    [ -n "$token" ] || return 0
    box_answer+=("$token")
  done
  return 1
}

# entry_state <path>: prints present when something is at <path>, a link whose target is gone
# included; absent when nothing is and the directory that would hold it was searched; and unknown
# and why otherwise, as for an ancestor the gate may not search or a link along the way whose target
# is gone, so a surface the gate cannot see is never read as one that is not there. A name that is
# no UTF-8, a link's target among them, is written with its bytes escaped, as Python writes an
# error, never a write that fails and stops the gate with no verdict.
entry_state() {
  if ! box_reader_call state "$1" || [ "${box_answer[0]}" != ok ]; then
    printf 'unknown %s: the gate'"'"'s path reader did not answer\n' "$1"
    return 0
  fi
  printf '%s\n' "${box_answer[1]}"
}

# utf8 <text>: true when <text> is UTF-8. A reader that does not answer is no proof that it is, so
# the caller names that path UNKNOWN, as it does for a name that is no UTF-8.
utf8() {
  box_reader_call utf8 "$1" && [ "${box_answer[0]}" = yes ]
}

# unresolved <link>: how resolving <link>, a link something is at, fails, as the words after "a link
# to <its target>, which". When nothing is at the end of its chain, it does not exist when nothing
# is at its target either, read from the link's own directory, and does not resolve when its target
# is there (a link whose target is gone). Any other error, such as a directory along the way the
# gate may not search or a loop, is named: does not resolve: <error>. A test of what is at the
# target reads each such error as nothing there. It fails when <link> resolves.
unresolved() {
  if ! box_reader_call unresolved "$1" || [ "${box_answer[0]}" = fail ]; then
    printf 'does not resolve: the gate'"'"'s path reader did not answer\n'
    return 0
  fi
  [ "${box_answer[0]}" = yes ] || return 1
  printf '%s\n' "${box_answer[1]}"
}

# plugin_trees <out>: the Oh My Pi plugin trees of this box, ~/.omp/plugins and each profile's, one
# per NUL-terminated entry, into <out>; a profiles directory it cannot list, a link there whose
# target is gone among them, fails, naming why, and so does a profile whose name is no UTF-8, which
# entry_state may not get unchanged (dotfiles_clients says why). A failure still writes the trees
# found before it, ~/.omp/plugins, so the caller reads that tree too and names every surface it
# could not see in one line.
plugin_trees() {
  local tree failed=0 trees=()
  : >"$1"
  if ! box_reader_call trees "$HOME"; then
    echo "the gate's path reader did not answer" >&2
    return 1
  fi
  if [ "${box_answer[0]}" = ok ]; then
    trees=("${box_answer[@]:1}")
  else
    failed=1
    trees=("${box_answer[@]:2}")
  fi
  for tree in "${trees[@]}"; do
    printf '%s\0' "$tree" >>"$1"
  done
  [ "$failed" = 1 ] || return 0
  printf '%s\n' "${box_answer[1]}" >&2
  return 1
}

# dotfiles_clients: the pi-envoy and claude-envoy of this developer box's dotfiles: the pins, and
# the copies installed from them. Every plugin tree it must hold to the dotfiles is under HOME, so a
# run with no HOME reads none of it.
dotfiles_clients() {
  if [ -z "${HOME:-}" ]; then
    echo "UNKNOWN: dotfiles: HOME is not set, so this box's plugin trees (~/.omp, ~/.claude) cannot be found"
    unknown=$((unknown + 1))
    return
  fi
  local root=$dotfiles tree state own home outside="" unread="" pin installed label problem name path why
  # The path reader opens here, in this shell, so every question below, each of them inside a
  # `$(...)`, writes to the pipes this shell holds.
  box_reader_start
  # The path reader and jq get each path read here as an argument or as a name on a pipe, and a
  # launcher that reads its arguments as text (a mise shim) hands on each byte that is no UTF-8 as
  # U+FFFD: the changed path is one where nothing is, which reads as absent. So a directory these
  # paths start from whose name is no UTF-8 is UNKNOWN by name, and plugin_trees refuses such a
  # profile.
  for name in HOME "the dotfiles directory" CLAUDE_CONFIG_DIR; do
    case "$name" in
      HOME) path=$HOME ;;
      CLAUDE_CONFIG_DIR) path=${CLAUDE_CONFIG_DIR:-} ;;
      *) path=$root ;;
    esac
    if [ -n "$path" ] && ! utf8 "$path"; then
      printf 'UNKNOWN: dotfiles: %s is no UTF-8, so a python3 or jq that reads its arguments as text may read another path: %s\n' "$name" "$(printable "$path")"
      unknown=$((unknown + 1))
      return
    fi
  done
  local package="$root/omp/plugins/node_modules/@sjawhar/pi-envoy"
  label="dotfiles omp/plugins/package.json"
  # Each copy an Oh My Pi plugin tree holds must be the dotfiles' own, a directory inside the
  # dotfiles once every link is resolved: `omp plugin link` puts a link to a checkout at that path,
  # and a tree linked to the dotfiles would carry it inside them.
  home=$(readlink -f "$root" || true)
  own=$(readlink -f "$package" || true)
  state=$(entry_state "$package")
  if [ "$state" = present ] && why=$(unresolved "$package"); then
    outside="$root/omp/plugins, a link to $(readlink "$package"), which $why"
  elif [ "$state" = present ] && [[ $own != "$home"/* ]]; then
    outside="$root/omp/plugins, a link to ${own:-a path that does not resolve}"
  elif [ "$state" != present ] && [ "$state" != absent ]; then
    unread="${state#unknown }"
  fi
  if ! plugin_trees "$work/trees" 2>"$work/err"; then
    unread+="${unread:+, }$(last_line "$work/err")"
  fi
  while IFS= read -r -d '' tree; do
    state=$(entry_state "$tree/node_modules/@sjawhar/pi-envoy")
    case "$state" in
      absent) ;;
      present)
        if [ ! -e "$tree/node_modules/@sjawhar/pi-envoy/package.json" ] ||
          [ "$(readlink -f "$tree/node_modules/@sjawhar/pi-envoy" || true)" != "$own" ]; then
          outside+="${outside:+, }$tree"
        fi
        ;;
      *) unread+="${unread:+, }${state#unknown }" ;;
    esac
  done <"$work/trees"
  if [ -n "$unread" ]; then
    printf 'UNKNOWN: %s: cannot tell whether @sjawhar/pi-envoy is installed: %s\n' "$label" "$(printable "$unread")"
    unknown=$((unknown + 1))
  elif [ -n "$outside" ]; then
    printf 'UNKNOWN: %s: @sjawhar/pi-envoy is installed outside %s, in %s\n' "$label" "$(printable "$root")" "$(printable "$outside")"
    unknown=$((unknown + 1))
  elif ! pin=$(jq -er '.dependencies["@sjawhar/pi-envoy"] | strings' "$root/omp/plugins/package.json" 2>"$work/err"); then
    problem=$(first_line "$work/err")
    printf 'UNKNOWN: %s: it pins no @sjawhar/pi-envoy version this gate can read%s\n' "$label" "${problem:+: $problem}"
    unknown=$((unknown + 1))
  else
    judge_release "$label" pi-envoy "$pin"
    # The copy installed from the pin is what a new session loads; one left behind the pin by an
    # install that did not finish is read on its own.
    if [ -e "$package" ]; then
      label="dotfiles omp/plugins installed @sjawhar/pi-envoy"
      if ! installed=$(jq -er 'select(.name == "@sjawhar/pi-envoy") | .version | strings' "$package/package.json" 2>/dev/null); then
        printf 'UNKNOWN: %s: %s/package.json is no @sjawhar/pi-envoy manifest with a version\n' "$label" "$(printable "$package")"
        unknown=$((unknown + 1))
      elif [ "$installed" != "$pin" ]; then
        judge_release "$label" pi-envoy "$installed" "; the pin names $(printable "$pin")"
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
  local root=$1 label="dotfiles .claude/settings.json" record state own_record=absent name version enabled others marketplace repo ref query plugin range packed served
  local extra file shown_file shown_range shown_plugin registry kind
  local -a registry_args=()
  for record in "$HOME/.claude/plugins/installed_plugins.json" ${CLAUDE_CONFIG_DIR:+"$CLAUDE_CONFIG_DIR/plugins/installed_plugins.json"} "$root/.claude/plugins/installed_plugins.json"; do
    state=$(entry_state "$record")
    if [ "$state" = absent ]; then
      continue
    elif [ "$state" != present ]; then
      printf 'UNKNOWN: %s: cannot tell whether a Claude Code plugin record is there: %s\n' "$label" "$(printable "${state#unknown }")"
      unknown=$((unknown + 1))
      return
    fi
    if [ "$record" = "$root/.claude/plugins/installed_plugins.json" ]; then
      own_record=present
      continue
    fi
    # A record is the dotfiles' own only when something is there and both paths resolve to it. A
    # link that loops, or whose target is gone, is read below and fails naming why, even one that
    # names the dotfiles' own path: two paths that resolve to one name where nothing is are no record.
    if [ -e "$record" ] && [ "$(readlink -f "$record")" = "$(readlink -f "$root/.claude/plugins/installed_plugins.json" || true)" ]; then
      continue
    fi
    if ! installed_claude_envoy "$record" >"$work/claude.outside" 2>"$work/err"; then
      printf 'UNKNOWN: %s: %s\n' "$label" "$(last_line "$work/err")"
      unknown=$((unknown + 1))
      return
    fi
    if [ -s "$work/claude.outside" ]; then
      printf 'UNKNOWN: %s: claude-envoy is installed outside %s, in %s\n' "$label" "$(printable "$root")" "$(printable "$(dirname "$record")")"
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
    printf 'UNKNOWN: %s: it enables %s, from a marketplace this gate does not read\n' "$label" "$(printable "$others")"
    unknown=$((unknown + 1))
    return
  fi
  if [ "$own_record" = present ]; then
    if ! installed_claude_envoy "$root/.claude/plugins/installed_plugins.json" "$root/.claude" >"$work/claude.installed" 2>"$work/err"; then
      printf 'UNKNOWN: %s: %s\n' "$label" "$(last_line "$work/err")"
      unknown=$((unknown + 1))
      return
    fi
    while IFS=$'\t' read -r name version recorded; do
      if [ "$name" != claude-envoy@legion-plugins ]; then
        printf 'UNKNOWN: %s: %s is installed, from a marketplace this gate does not read\n' "$label" "$name"
        unknown=$((unknown + 1))
      else
        judge_release "dotfiles .claude/plugins installed claude-envoy" claude-envoy "$version" "${recorded:+; the plugin record says $recorded}"
      fi
    done <"$work/claude.installed"
  fi
  if [ "$enabled" != true ]; then
    printf 'SKIPPED: %s enables no claude-envoy@legion-plugins\n' "$label"
    return
  fi
  # Claude Code reads a github marketplace's repo, ref and path (the marketplace file's path in the
  # repository, .claude-plugin/marketplace.json when none is set), and an npm plugin's package,
  # version and registry (https://code.claude.com/docs/en/plugins/marketplace-reference). The gate
  # reads each as Claude Code does, and a key it does not read is UNKNOWN, since it may move what is
  # installed.
  if ! marketplace=$(jq -c '.extraKnownMarketplaces["legion-plugins"].source // {}' "$root/.claude/settings.json" 2>"$work/err"); then
    printf 'UNKNOWN: %s: cannot read the marketplace legion-plugins it names: %s\n' "$label" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if [ "$(jq -r '.source // ""' <<<"$marketplace")" != github ] || ! repo=$(jq -er '.repo | strings' <<<"$marketplace") ||
    ! [[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
    ! jq -e '(.ref // "" | type == "string") and (.path // "" | type == "string")' <<<"$marketplace" >/dev/null; then
    printf 'UNKNOWN: %s: the marketplace legion-plugins it names is %s, which is no GitHub repository this gate reads\n' "$label" "$(printable "$marketplace")"
    unknown=$((unknown + 1))
    return
  fi
  extra=$(jq -r 'keys_unsorted - ["source", "repo", "ref", "path"] | first // ""' <<<"$marketplace")
  if [ -n "$extra" ]; then
    printf 'UNKNOWN: %s: the marketplace legion-plugins it names sets %s, which this gate does not read\n' "$label" "$(printable "$extra")"
    unknown=$((unknown + 1))
    return
  fi
  # A `..` or `.` segment, or an empty one, would let GitHub's API (or the URL it is sent as) read a
  # file the path does not name in the repository Claude Code clones.
  if ! jq -e '.path // ".claude-plugin/marketplace.json" | split("/") | all(. != "" and . != "." and . != "..")' <<<"$marketplace" >/dev/null; then
    printf 'UNKNOWN: %s: the marketplace legion-plugins it names sets path %s, which is no plain path inside its repository\n' "$label" "$(printable "$(jq -c '.path' <<<"$marketplace")")"
    unknown=$((unknown + 1))
    return
  fi
  ref=$(jq -r '.ref // ""' <<<"$marketplace")
  file=$(jq -r '.path // ".claude-plugin/marketplace.json"' <<<"$marketplace")
  shown_file=$(printable "$file")
  # The ref is one query value and each segment of the path one path segment, so every character a
  # URL reserves in them is percent-encoded.
  query=""
  [ -z "$ref" ] || query="?ref=$(jq -rn --arg ref "$ref" '$ref | @uri')"
  if ! gh api -H "Accept: application/vnd.github.raw+json" "repos/$repo/contents/$(jq -rn --arg file "$file" '$file | split("/") | map(@uri) | join("/")')${query}" >"$work/marketplace.json" 2>"$work/err"; then
    printf 'UNKNOWN: %s: cannot read %s %s%s: %s\n' "$label" "$repo" "$shown_file" "${ref:+ at $(printable "$ref")}" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  if ! plugin=$(jq -ec '[.plugins[] | select(.name == "claude-envoy")] | if length == 1 then .[0].source else error("\(length) entries name claude-envoy") end' "$work/marketplace.json" 2>"$work/err") ||
    [ "$(jq -r '.source? // ""' <<<"$plugin")" != npm ] ||
    [ "$(jq -r '.package? // ""' <<<"$plugin")" != "@sjawhar/claude-envoy" ] ||
    ! jq -e '.registry // "" | type == "string"' <<<"$plugin" >/dev/null; then
    if [ -n "$plugin" ]; then shown_plugin=$(printable "$plugin"); else shown_plugin=$(first_line "$work/err"); fi
    printf 'UNKNOWN: %s: %s %s serves claude-envoy as %s, not as one entry of the npm package @sjawhar/claude-envoy\n' "$label" "$repo" "$shown_file" "$shown_plugin"
    unknown=$((unknown + 1))
    return
  fi
  extra=$(jq -r 'keys_unsorted - ["source", "package", "version", "registry"] | first // ""' <<<"$plugin")
  if [ -n "$extra" ]; then
    printf 'UNKNOWN: %s: %s %s serves claude-envoy with %s, which this gate does not read\n' "$label" "$repo" "$shown_file" "$(printable "$extra")"
    unknown=$((unknown + 1))
    return
  fi
  range=$(jq -r '.version // "latest"' <<<"$plugin")
  shown_range=$(printable "$range")
  registry=$(jq -r '.registry // ""' <<<"$plugin")
  registry_args=()
  # Claude Code takes a registry other than the box's default only over https, and npm answers from
  # its default registry, with a warning on stderr, for a value it cannot read as a URL.
  if [ -n "$registry" ] && ! [[ $registry =~ ^https://[^[:space:]]+$ ]]; then
    printf 'UNKNOWN: %s: %s %s serves claude-envoy from registry %s, which is no https URL\n' "$label" "$repo" "$shown_file" "$(printable "$registry")"
    unknown=$((unknown + 1))
    return
  fi
  [ -z "$registry" ] || registry_args=("--registry=$registry")
  # The marketplace's version is the spec npm installs from, and only a registry spec (a range, an
  # exact version or a dist-tag) names a release. npm builds any other spec (a directory, a git
  # repository, a tarball URL, an alias) from its source, running its `prepare` script even under
  # --ignore-scripts (pacote 19.0.2, lib/dir.js:30-45, in npm 10.9.7), and its version comes from
  # that source, not from a release; so such a spec is UNKNOWN before npm fetches it. npm's own
  # parser classifies it: the npm-package-arg npm ships, found beside npm's npm-cli.js, which npm
  # exec gives a command as npm_execpath, run by the node npm runs on, npm_node_execpath
  # (@npmcli/config lib/set-envs.js:104-105).
  # shellcheck disable=SC2016 # npm exec's shell expands them, with the values npm gives it
  if ! kind=$(cd "$work" && env GATE_SPEC="@sjawhar/claude-envoy@$range" GATE_SPEC_TYPE="$spec_type_js" \
    npm exec --offline --yes=false -c '"$npm_node_execpath" -e "$GATE_SPEC_TYPE"' 2>"$work/err"); then
    printf 'UNKNOWN: %s: npm cannot parse @sjawhar/claude-envoy@%s: %s\n' "$label" "$shown_range" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  case "$kind" in
    range | version | tag) ;;
    *)
      printf 'UNKNOWN: %s: legion-plugins serves claude-envoy as @sjawhar/claude-envoy@%s, a %s spec, which npm builds from its source and no release names\n' \
        "$label" "$shown_range" "$(printable "$kind")"
      unknown=$((unknown + 1))
      return
      ;;
  esac
  # npm picks the version, as an install on this box does: `npm pack <spec> --dry-run --json` runs
  # npm's own pick (npm-pick-manifest) under this box's npm configuration — the dist-tag its `tag`
  # setting names, `before` (and on npm 11 `min-release-age`), deprecation, and the engines of the
  # node and npm it runs on — and answers the one version an install takes, so the gate keeps no
  # pick of its own to fall behind npm's rules. It runs in $work, a directory with no package.json
  # or .npmrc, so no project's npm configuration applies: a plugin is installed outside any
  # project. The dry run fetches the tarball into npm's cache and writes nothing else, and
  # --ignore-scripts keeps npm from running any script of the package.
  if ! packed=$(cd "$work" && npm pack "@sjawhar/claude-envoy@$range" --dry-run --ignore-scripts --json "${registry_args[@]}" 2>"$work/err"); then
    printf 'UNKNOWN: %s: npm cannot resolve @sjawhar/claude-envoy@%s: %s\n' "$label" "$shown_range" "$(first_line "$work/err")"
    unknown=$((unknown + 1))
    return
  fi
  # npm answers a list of the one package it packed, holding its name and version.
  if ! served=$(jq -er 'if type == "array" and length == 1 and (.[0] | type == "object")
      and .[0].name == "@sjawhar/claude-envoy" then .[0].version | strings else empty end' <<<"$packed" 2>/dev/null); then
    printf 'UNKNOWN: %s: npm answers no version of @sjawhar/claude-envoy for %s\n' "$label" "$shown_range"
    unknown=$((unknown + 1))
    return
  fi
  judge_release "$label" claude-envoy "$served" "; the version legion-plugins serves for $shown_range"
}
