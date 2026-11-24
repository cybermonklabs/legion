# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-read-vm-hub.sh
#
# The --listeners leg's reader of the VM hub, which daemon-release-gate.sh sources; it is not run on
# its own. It reads the gate's deployment, work and first_line and last_line, and sets hub, hub_use,
# hub_how and hub_head for the gate.
# shellcheck disable=SC2154 # deployment and work are the gate's
# shellcheck disable=SC2034 # hub, hub_use, hub_how and hub_head are the gate's to read

# vm_hub decides whether the deployment repository declares the VM hub, gcp:NatsHub, from its program
# at one commit, the head of its default branch, which it sets hub_head to: hub yes, hub_use the file
# and line of a declaration and hub_how what it does there, when that code calls NewNatsHub or
# registers the hub's type token itself; and hub no when every Go file of it was read and none does;
# it returns 0. It reads the whole repository in one archive (`gh api
# repos/<deployment>/tarball/<sha>`) and every Go file in it but a _test.go, which no build of a
# program compiles: envoy/, every directory it imports or a go.mod or go.work replaces with,
# testdata/ and _ directories included, so no rule of which directories a build reads can leave a
# use unread, and a use outside envoy/ reads as one in it, which only ever reads the VM pin.
#
# The archive alone cannot show what it lacks: GitHub makes it as `git archive` does, which leaves
# out each path a .gitattributes marks export-ignore and rewrites each it marks export-subst, and a
# .gitattributes can mark itself. So the gate also reads the commit's tree (`gh api
# repos/<deployment>/git/trees/<sha>?recursive=1`) and holds the archive to it: each Go file and
# module file the tree holds must be in the archive, and each file the gate reads (a Go file, a
# module file, a link and the file it links to) must be the tree's blob, byte for byte, by git's own
# hash of it. A tree GitHub lists only in part (`truncated`), and a submodule (a tree entry of type
# commit), whose code the archive does not hold, are UNKNOWN.
#
# A Go file that is a link is read at its target. A use is the identifier NewNatsHub in code, outside
# comments and strings, anywhere but as the name of the func that defines it, which Go declares only
# at the top level, outside every brace; or a string that is the hub's type token,
# envoy-infra:gcp:NatsHub, written as one literal, which code registers the hub by when it calls no
# NewNatsHub. The generated SDK's own registration, inside its NewNatsHub, and its switch of the
# types it rebuilds (a `case` label) declare no hub. A file that names either and that the gate
# cannot lex as Go text (no UTF-8, or a comment or string with no end) is UNKNOWN; a file that names
# neither cannot use them, and an error of Go syntax cannot turn code into a comment or a string. A
# link that leaves the repository, a Go file that is a link to no file of it, and an archive member
# git never writes (a hard link, a second copy of one path, a directory or device under a Go file's
# name) are UNKNOWN, since the gate cannot read what they name; so are a repository that declares
# submodules (.gitmodules), a local replace or use of envoy/go.mod, or of a go.work at the
# repository's root or in envoy/, that leaves the repository, a head that cannot be read or is no
# commit id, a tree or an archive it cannot fetch or read, and an envoy/ with no Go file or no
# go.mod. A module fetched from outside the repository is not read. Each of those prints its UNKNOWN
# line and returns 1. Every name the repository gives is printed with its control characters written
# out (printable). The pin is never the evidence: the hub's removal deletes its config keys too, so
# an absent listenerImage cannot tell a gone hub from a stack file that lost its pin.
vm_hub() {
  hub=""
  hub_use=""
  hub_how=""
  if ! hub_head=$(gh api "repos/${deployment}/commits/HEAD" --jq .sha 2>"$work/err"); then
    printf 'UNKNOWN: envoy: cannot read the head of %s: %s\n' "$deployment" "$(first_line "$work/err")"
    return 1
  fi
  if ! [[ $hub_head =~ ^[0-9a-f]{40}$ ]]; then
    printf 'UNKNOWN: envoy: GitHub answered %s for the head of %s, which is no commit id\n' "$(printable "$hub_head")" "$deployment"
    return 1
  fi
  if ! gh api "repos/${deployment}/git/trees/${hub_head}?recursive=1" >"$work/tree.json" 2>"$work/err"; then
    printf 'UNKNOWN: envoy: cannot read the tree of %s at %s: %s\n' "$deployment" "$hub_head" "$(first_line "$work/err")"
    return 1
  fi
  if ! gh api "repos/${deployment}/tarball/${hub_head}" >"$work/repo.tgz" 2>"$work/err"; then
    printf 'UNKNOWN: envoy: cannot fetch %s at %s: %s\n' "$deployment" "$hub_head" "$(first_line "$work/err")"
    return 1
  fi
  if ! python3 - "$work/repo.tgz" "$work/tree.json" >"$work/go.hub" 2>"$work/err" <<'PY'; then
import ast
import bisect
import hashlib
import json
import os
import posixpath
import re
import sys
import tarfile

TOKEN = "envoy-infra:gcp:NatsHub"


def shown(text):
    """text with each character that is not printable written as \\x<hh>, \\u<hhhh> or \\U<hhhhhhhh>."""
    def one(c):
        if c.isprintable():
            return c
        n = ord(c)
        return "\\x%02x" % n if n < 0x100 else "\\u%04x" % n if n < 0x10000 else "\\U%08x" % n

    return "".join(one(c) for c in text)


def fail(message):
    """exits with message on one line, every name in it printable."""
    sys.exit(shown(message))


# The commit's tree, which the archive is held to. GitHub lists a large tree only in part.
try:
    with open(sys.argv[2], encoding="utf-8") as handle:
        tree = json.load(handle)
except (OSError, UnicodeDecodeError, ValueError) as error:
    fail(f"GitHub's tree of the commit cannot be read: {error}")
if not isinstance(tree, dict) or not isinstance(tree.get("tree"), list):
    fail("GitHub's answer for the commit's tree is no tree listing")
if tree.get("truncated") is not False:
    fail("GitHub lists the commit's tree only in part, so what the archive leaves out cannot be told")
blobs = {}
for entry in tree["tree"]:
    if not isinstance(entry, dict) or not isinstance(entry.get("path"), str):
        fail("GitHub's tree of the commit holds an entry with no path")
    if entry.get("type") == "commit":
        fail(f"the commit holds {entry['path']} as a submodule, whose code the archive does not hold")
    if entry.get("type") == "blob":
        blobs[entry["path"]] = entry.get("sha")


def blob_id(data):
    """git's hash of data as a blob, the id the tree names a file or a link's target by."""
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def held(path, data):
    """exits unless the archive's bytes at path are the commit's own."""
    if path not in blobs:
        fail(f"the archive holds {path}, which the commit's tree does not")
    if blob_id(data) != blobs[path]:
        fail(f"the archive's {path} is not the commit's (an export-subst or a conversion changed it), so its code cannot be read")


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
                fail(f"the archive holds {name} twice, which a repository cannot")
            # GitHub's archive holds a submodule as an empty directory, so its code is never read.
            if name == ".gitmodules":
                fail("the repository holds .gitmodules, and submodule code is not in the archive")
            if member.islnk():
                fail(f"the archive holds {name} as a hard link, which git never writes")
            if wanted(name) and not (member.isfile() or member.issym()):
                fail(f"the archive holds {name} as no file and no link")
            members[name] = (member, archive.extractfile(member).read() if member.isfile() and wanted(name) else None)
        # A regular file a Go file links to is read too, whatever its own name.
        links = {name: member.linkname for name, (member, _) in members.items() if member.issym()}

        def resolve(path, depth=0):
            """path with every link along it followed, or an exit when a link leaves the repository."""
            if depth > 40:
                fail(f"the links at {path} make a loop")
            parts = path.split("/")
            for i in range(1, len(parts) + 1):
                prefix = "/".join(parts[:i])
                if prefix in links:
                    target = links[prefix]
                    joined = posixpath.normpath(posixpath.join(posixpath.dirname(prefix), target))
                    if target.startswith("/") or joined == ".." or joined.startswith("../"):
                        fail(f"{prefix} is a link to {target}, outside this repository")
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
                        fail(f"{name} is a link to {member.linkname}, which is no file of this repository")
                    held(name, os.fsencode(member.linkname))
                    sources[name] = archive.extractfile(found[0]).read()
                    held(target, sources[name])
            elif data is not None:
                held(name, data)
                sources[name] = data
except (OSError, EOFError, tarfile.TarError) as error:
    fail(f"the archive cannot be read: {error}")
for path in sorted(blobs):
    if wanted(path) and path not in members:
        fail(f"the archive leaves out {path}, which the commit holds, so its code cannot be read")
# A file stored in Git LFS is its pointer in both the tree and the archive, and GitHub's archive
# leaves the object out, so its code is never there to read.
for name in sorted(sources):
    if sources[name].startswith(b"version https://git-lfs.github.com/spec/v1"):
        fail(f"{name} is a Git LFS pointer, whose content neither the tree nor the archive holds, so its code cannot be read")


def go_files(top):
    return sorted(n for n in sources if n.startswith(top) and n.endswith(".go"))


if not go_files("envoy/"):
    fail("envoy/ holds no Go file to read the VM hub from")


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
                fail(f"{name} has a string with no end")
            try:
                out.append(json.loads(line[i : j + 1]))
            except ValueError:
                fail(f"{name} has a string this gate cannot decode: {line[i : j + 1]}")
            i = j + 1
        elif line[i] == "`":
            j = line.find("`", i + 1)
            if j < 0:
                fail(f"{name} has a raw string with no end")
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
        fail(f"{name} is no UTF-8 text")
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
                fail(f"{name} has a replace this gate cannot read: {line.strip()}")
            path = spec[spec.index("=>") + 1]
        else:
            path = spec[0]
        if kind == "use" or path in (".", "..") or path.startswith(("./", "../", "/")) or "\\" in path:
            yield path


if "envoy/go.mod" not in sources:
    fail("envoy/ holds no go.mod")
for name in modules:
    for path in local_paths(name) if name in sources else ():
        joined = posixpath.normpath(posixpath.join(posixpath.dirname(name), path))
        if path.startswith("/") or "\\" in path or joined == ".." or joined.startswith("../"):
            fail(f"{name} names {path}, a directory outside this repository")


def code(text, name, named):
    """text with every comment and string blanked out, newlines kept, and each string's offset and
    value, its escapes decoded where Go and Python write them alike."""
    out, strings, i, n = [], [], 0, len(text)
    while i < n:
        if text.startswith("//", i):
            j = text.find("\n", i)
            j = n if j < 0 else j
        elif text.startswith("/*", i):
            j = text.find("*/", i + 2)
            if j < 0:
                fail(f"{name} names {named} and has a /* comment with no end")
            j += 2
        elif text[i] == "`":
            j = text.find("`", i + 1)
            if j < 0:
                fail(f"{name} names {named} and has a raw string with no end")
            j += 1
            strings.append((i, text[i + 1 : j - 1].replace("\r", "")))
        elif text[i] in "\"'":
            j = i + 1
            while j < n and text[j] not in (text[i], "\n"):
                j += 2 if text[j] == "\\" else 1
            if j >= n or text[j] != text[i]:
                fail(f"{name} names {named} and has a string with no end")
            j += 1
            if text[i] == '"':
                try:
                    strings.append((i, ast.literal_eval(text[i:j])))
                except (ValueError, SyntaxError):
                    strings.append((i, text[i + 1 : j - 1]))
        else:
            out.append(text[i])
            i += 1
            continue
        out.append(re.sub(r"[^\n]", " ", text[i:j]))
        i = j
    return "".join(out), strings


def declared_name(before):
    """whether the code before an identifier makes it the name of a func being declared. Go declares
    a func only at the top level, so a name inside braces never is one: there `func(ctx int)` ends a
    type, and Go ends the line after its `)`."""
    if before.count("{") > before.count("}"):
        return False
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


def declarations(source):
    """the offset and keyword of each declaration at the top level, outside every bracket."""
    found, depth = [], 0
    for match in re.finditer(r"[{([]|[})\]]|(?<!\w)(?:func|var|const|type|import)(?!\w)", source):
        word = match.group()
        if word in "{([":
            depth += 1
        elif word in "})]":
            depth -= 1
        elif depth == 0:
            found.append((match.start(), word))
    return found


def sdk_registration(source, decls, offset):
    """whether the string at offset is the SDK's own registration, inside its NewNatsHub, or a case
    label of the SDK's switch of the types it rebuilds: neither declares a hub."""
    line = source[source.rfind("\n", 0, offset) + 1 : offset]
    if re.match(r"\s*case(?!\w)", line):
        return True
    index = bisect.bisect_left([start for start, _ in decls], offset) - 1
    if index < 0 or decls[index][1] != "func":
        return False
    return re.match(r"func\s*(?:\([^)]*\)\s*)?NewNatsHub(?!\w)", source[decls[index][0] :]) is not None


uses = []
for name in go_files(""):
    raw = sources[name]
    if b"NewNatsHub" not in raw and TOKEN.encode() not in raw:
        continue
    named = "NewNatsHub" if b"NewNatsHub" in raw else TOKEN
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        fail(f"{name} names {named} and is no UTF-8 text")
    source, strings = code(text, name, named)
    found = [(use.start(), "uses NewNatsHub") for use in re.finditer(r"(?<!\w)NewNatsHub(?!\w)", source) if not declared_name(source[: use.start()])]
    decls = declarations(source)
    found += [(offset, f"registers {TOKEN}") for offset, value in strings if value == TOKEN and not sdk_registration(source, decls, offset)]
    for offset, how in sorted(found):
        uses.append((f"{name}:{source.count(chr(10), 0, offset) + 1}", how))
print(f"yes\t{shown(uses[0][0])}\t{uses[0][1]}" if uses else "no")
PY
    printf 'UNKNOWN: envoy: cannot read the Go files of %s at %s: %s\n' "$deployment" "$hub_head" "$(last_line "$work/err")"
    return 1
  fi
  IFS=$'\t' read -r hub hub_use hub_how <"$work/go.hub"
}
