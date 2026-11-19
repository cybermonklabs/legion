# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-read-vm-hub.sh
#
# The --listeners leg's reader of the VM hub, which daemon-release-gate.sh sources; it is not run on
# its own. It reads the gate's deployment, work and first_line and last_line, and sets hub, hub_use
# and hub_head for the gate.
# shellcheck disable=SC2154 # deployment and work are the gate's
# shellcheck disable=SC2034 # hub, hub_use and hub_head are the gate's to read

# vm_hub decides whether the deployment repository declares the VM hub, gcp:NatsHub, from its program
# at one commit, the head of its default branch, which it sets hub_head to: hub yes and hub_use the
# file and line of a use when that code uses NewNatsHub, and hub no when every Go file of it was read
# and none does; it returns 0. It reads the whole repository in one archive (`gh api
# repos/<deployment>/tarball/<sha>`) and every Go file in it but a _test.go, which no build of a
# program compiles: envoy/, every directory it imports or a go.mod or go.work replaces with,
# testdata/ and _ directories included, so no rule of which directories a build reads can leave a
# use unread, and a use outside envoy/ reads as one in it, which only ever reads the VM pin. A Go
# file that is a link is read at its target. A use is the identifier NewNatsHub in code, outside
# comments and strings, anywhere but as the name of the func that defines it. A file that names
# NewNatsHub and that the gate cannot lex as Go text (no UTF-8, or a comment or string with no end)
# is UNKNOWN; a file that does not name it cannot use it, and an error of Go syntax cannot turn code
# into a comment or a string. A link that leaves the repository, a Go file that is a link to no file
# of it, and an archive member git never writes (a hard link, a second copy of one path, a directory
# or device under a Go file's name) are UNKNOWN, since the gate cannot read what they name; so are a
# local replace or use of envoy/go.mod, or of a go.work at the repository's root or in envoy/, that
# leaves the repository, a head that cannot be read or is no commit id, an archive it cannot fetch
# or unpack, and an envoy/ with no Go file or no go.mod. A module fetched from outside the
# repository is not read. Each of those prints its UNKNOWN line and returns 1. The pin is never the
# evidence: the hub's removal deletes its config keys too, so an absent listenerImage cannot tell a
# gone hub from a stack file that lost its pin.
vm_hub() {
  hub=""
  hub_use=""
  if ! hub_head=$(gh api "repos/${deployment}/commits/HEAD" --jq .sha 2>"$work/err"); then
    printf 'UNKNOWN: envoy: cannot read the head of %s: %s\n' "$deployment" "$(first_line "$work/err")"
    return 1
  fi
  if ! [[ $hub_head =~ ^[0-9a-f]{40}$ ]]; then
    printf 'UNKNOWN: envoy: GitHub answered %s for the head of %s, which is no commit id\n' "$hub_head" "$deployment"
    return 1
  fi
  if ! gh api "repos/${deployment}/tarball/${hub_head}" >"$work/repo.tgz" 2>"$work/err"; then
    printf 'UNKNOWN: envoy: cannot fetch %s at %s: %s\n' "$deployment" "$hub_head" "$(first_line "$work/err")"
    return 1
  fi
  if ! python3 - "$work/repo.tgz" >"$work/go.hub" 2>"$work/err" <<'PY'; then
import json
import posixpath
import re
import sys
import tarfile

# Every Go file but the tests, and the go.mod and go.work files a local replace or use is read from,
# by its path in the repository: GitHub's archive holds the repository under one top directory.
modules = ("envoy/go.mod", "go.work", "envoy/go.work")


def wanted(name):
    return (name.endswith(".go") and not name.endswith("_test.go")) or name in modules


members = {}
try:
    with tarfile.open(sys.argv[1], "r:gz") as archive:
        for member in archive:
            name = member.name.partition("/")[2].rstrip("/")
            if not name:
                continue
            if name in members:
                sys.exit(f"the archive holds {name} twice, which a repository cannot")
            if member.islnk():
                sys.exit(f"the archive holds {name} as a hard link, which git never writes")
            if wanted(name) and not (member.isfile() or member.issym()):
                sys.exit(f"the archive holds {name} as no file and no link")
            members[name] = (member, archive.extractfile(member).read() if member.isfile() and wanted(name) else None)
        # A regular file a Go file links to is read too, whatever its own name.
        links = {name: member.linkname for name, (member, _) in members.items() if member.issym()}

        def resolve(path, depth=0):
            """path with every link along it followed, or an exit when a link leaves the repository."""
            if depth > 40:
                sys.exit(f"the links at {path} make a loop")
            parts = path.split("/")
            for i in range(1, len(parts) + 1):
                prefix = "/".join(parts[:i])
                if prefix in links:
                    target = links[prefix]
                    joined = posixpath.normpath(posixpath.join(posixpath.dirname(prefix), target))
                    if target.startswith("/") or joined == ".." or joined.startswith("../"):
                        sys.exit(f"{prefix} is a link to {target}, outside this repository")
                    rest = parts[i:]
                    return resolve("/".join(([] if joined == "." else [joined]) + rest), depth + 1)
            return path

        sources = {}
        for name, (member, data) in members.items():
            if member.issym():
                target = resolve(name)
                if wanted(name):
                    found = members.get(target)
                    if found is None or not found[0].isfile():
                        sys.exit(f"{name} is a link to {member.linkname}, which is no file of this repository")
                    sources[name] = archive.extractfile(found[0]).read()
            elif data is not None:
                sources[name] = data
except (OSError, EOFError, tarfile.TarError) as error:
    sys.exit(f"the archive cannot be read: {error}")


def go_files(top):
    return sorted(n for n in sources if n.startswith(top) and n.endswith(".go"))


if not go_files("envoy/"):
    sys.exit("envoy/ holds no Go file to read the VM hub from")


def tokens(line, name):
    """the tokens of one line of a go.mod or go.work, quoted strings decoded, comments dropped."""
    out, i = [], 0
    while i < len(line):
        if line[i].isspace():
            i += 1
        elif line.startswith("//", i):
            break
        elif line[i] in "()":
            out.append(line[i])
            i += 1
        elif line[i] == '"':
            j = i + 1
            while j < len(line) and line[j] != '"':
                j += 2 if line[j] == "\\" else 1
            if j >= len(line):
                sys.exit(f"{name} has a string with no end")
            try:
                out.append(json.loads(line[i : j + 1]))
            except ValueError:
                sys.exit(f"{name} has a string this gate cannot decode: {line[i : j + 1]}")
            i = j + 1
        elif line[i] == "`":
            j = line.find("`", i + 1)
            if j < 0:
                sys.exit(f"{name} has a raw string with no end")
            out.append(line[i + 1 : j])
            i = j + 1
        else:
            j = i
            while j < len(line) and not line[j].isspace() and line[j] not in "()\"`":
                j += 1
            out.append(line[i:j])
            i = j
    return out


def local_paths(name):
    """each directory a replace or use of the go.mod or go.work at name names on disk."""
    try:
        text = sources[name].decode("utf-8")
    except UnicodeDecodeError:
        sys.exit(f"{name} is no UTF-8 text")
    block = None
    for line in text.splitlines():
        words = tokens(line, name)
        if block:
            if words == [")"]:
                block = None
                continue
            kind, spec = block, words
        elif words[:1] in (["replace"], ["use"]):
            if words[1:] == ["("]:
                block = words[0]
                continue
            kind, spec = words[0], words[1:]
        else:
            continue
        if not spec:
            continue
        if kind == "replace":
            if "=>" not in spec or spec.index("=>") == len(spec) - 1:
                sys.exit(f"{name} has a replace this gate cannot read: {line.strip()}")
            path = spec[spec.index("=>") + 1]
        else:
            path = spec[0]
        if kind == "use" or path in (".", "..") or path.startswith(("./", "../", "/")) or "\\" in path:
            yield path


if "envoy/go.mod" not in sources:
    sys.exit("envoy/ holds no go.mod")
for name in modules:
    for path in local_paths(name) if name in sources else ():
        joined = posixpath.normpath(posixpath.join(posixpath.dirname(name), path))
        if path.startswith("/") or "\\" in path or joined == ".." or joined.startswith("../"):
            sys.exit(f"{name} names {path}, a directory outside this repository")


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
                sys.exit(f"{name} names NewNatsHub and has a /* comment with no end")
            j += 2
        elif text[i] == "`":
            j = text.find("`", i + 1)
            if j < 0:
                sys.exit(f"{name} names NewNatsHub and has a raw string with no end")
            j += 1
        elif text[i] in "\"'":
            j = i + 1
            while j < n and text[j] not in (text[i], "\n"):
                j += 2 if text[j] == "\\" else 1
            if j >= n or text[j] != text[i]:
                sys.exit(f"{name} names NewNatsHub and has a string with no end")
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


uses = []
for name in go_files(""):
    raw = sources[name]
    if b"NewNatsHub" not in raw:
        continue
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        sys.exit(f"{name} names NewNatsHub and is no UTF-8 text")
    source = code(text, name)
    for use in re.finditer(r"(?<!\w)NewNatsHub(?!\w)", source):
        if not declared_name(source[: use.start()]):
            uses.append(f"{name}:{source.count(chr(10), 0, use.start()) + 1}")
print(f"yes {uses[0]}" if uses else "no")
PY
    printf 'UNKNOWN: envoy: cannot read the Go files of %s at %s: %s\n' "$deployment" "$hub_head" "$(last_line "$work/err")"
    return 1
  fi
  read -r hub hub_use <"$work/go.hub"
}
