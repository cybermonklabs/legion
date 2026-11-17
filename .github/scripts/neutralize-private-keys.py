#!/usr/bin/env python3
"""Replace each issue key of the deployment's own Dispatch projects in this tree with a placeholder.

This repository is public, and the deployment's project keys are private: AGENTS.md's "This
repository is public" rule names neither them nor their issue keys anywhere in the tree. The
keys come from the deployment's term list, the `PRIVATE_NAME_TERMS` variable: each line of the
form `key:<KEY>` (capitals and digits, the blanks around the line dropped) names one project,
`LEGION` never among them. Every other line of that list is a private name, which this script
does not replace. The list itself is private and is never written into the tree.

Each project key K has one placeholder P(K): as many capital letters as K has characters, the
n-th being "A" plus the n-th byte of SHA-256("neutral-project-key:v1:" + K) modulo 26. The
placeholder depends on K alone, so a key added to the list later changes no placeholder already
in the tree, and two runs over the same text write the same bytes. It is as long as the key, so a
column gofmt aligns, a line a formatter wraps and a byte offset a test counts stay where they
were. The script refuses a list in which two keys share a placeholder, a placeholder is itself a
key of the list or LEGION, or a placeholder holds a term of the list.

What it rewrites, in the contents of every text file and in every path:
  * an issue key, K-<n> as a word of its own, in capitals as Dispatch prints it, wherever it
    stands: in prose, in backticks, in a link's text or address, in a comment or a string of any
    language. The number is kept: K-<n> becomes P(K)-<n>;
  * the project of a Dispatch link: the scheme, then K, then anything but a letter, a digit or
    "_" (the link to a project's document, whose key has no number);
  * a quoted project key: K alone between two of the same quote ("K", 'K', `K`), the form a
    fixture or a test gives a project, so that its issues and its project stay one project;
  * K where Dispatch's, Envoy's or Legion's own syntax puts a project, so that a subject, a role
    or a search a test builds from a project still names the project its fixture gave:
    a token of a `notifications.` subject (notifications.dispatch.issue.K.<key>.>); the
    project of a role (K.legion-<name>, K.<role>, legion-K-<name>, K-controller and the other
    role words); a key a program builds or escapes (K-%d, K-${n}, K-" + n, K\\-<n>); a search
    form (K <n>, K:<n>); and a project after `project:`, `daemon:`, `project=`, `/projects/` and
    the label `Project ` or `project `;
  * the lower-case token Legion writes a project as, in a Legion role's name (legion-k-controller,
    legion-k-k-12-architect, notifications.role.legion-k-…), which takes the placeholder in lower
    case, since Legion reads the token's upper case back as the project.
It leaves every other text as it is: a link to a LEGION-<n> issue or a placeholder project, the
bare scheme, a lower-case slug outside a role's name, and K as a word in prose or a value of its
own (a key that is also an English or a technical word cannot be told from it), which --words
replaces, with a lower-case issue slug k-<n>, in the files a person has read; --lower-words also
replaces k as a lower-case word there (a Legion project token, "k"), for a file where every such
word is the project.

The files are these: with no PATH, every file git tracks and every
untracked file git does not ignore; with PATHs, every file under them. A symbolic link is not
followed, and a lockfile (bun.lock, go.sum, go.work.sum) and a file holding a NUL byte are left
as they are. A path that holds a key is renamed (git mv for a tracked file), and the run refuses
to rename onto a path that exists.

What KEEP_PATHS names stays as it is, because a program reads it against the deployment's own
Dispatch, where a placeholder names nothing, until it leaves the tree for a private store.

Usage: PRIVATE_NAME_TERMS="$(cat <the deployment's term list>)" \\
         .github/scripts/neutralize-private-keys.py [--check] [PATH...]
       ... .github/scripts/neutralize-private-keys.py --words|--lower-words PATH...

Without --check it rewrites the tree and exits 0; a second run changes nothing. With --check it
writes nothing and prints each path and line it would change, exiting 1 if there is one and 0 if
there is none. It never prints a key, a term or a key's placeholder beside its key: a path it
prints is the path after the rewrite. --words also replaces K as a word of its own, and only in
the PATHs it is given, each of which a person has read and found to hold no other word of those
letters. Exit 2 is a refusal: no `key:` line in the list, a placeholder clash, a rename onto an
existing path, or a git or file error.
"""

from __future__ import annotations

import hashlib
import os
import re
import subprocess
import sys
from pathlib import Path

LOCKFILES = {"bun.lock", "go.sum", "go.work.sum"}
# What a program reads against the deployment's own Dispatch as it is, until it leaves the tree:
# the approval experiment, whose held-out reserve is a registered hash over the deployment's real
# issue keys and whose tests are calibrated on them. A path ending in "/" keeps everything under
# it.
KEEP_PATHS = ("scripts/approval-eval/",)
SALT = b"neutral-project-key:v1:"
# The scheme of a Dispatch link, in pieces so that this file holds no whole one.
SCHEME = b"dispatch" + b":" + b"//"
# The blanks bash's [[:space:]] holds, trimmed around each line of the list.
BLANKS = " \t\n\r\v\f"
KEY_LINE = re.compile(r"key:([A-Z][A-Z0-9]*)")


def refuse(message: str) -> None:
    print(f"neutralize-private-keys: {message}", file=sys.stderr)
    sys.exit(2)


def read_list(value: str) -> tuple[list[str], list[str]]:
    """The project keys and the other lines of a PRIVATE_NAME_TERMS value, each line trimmed of
    the blanks around it."""
    keys: list[str] = []
    terms: list[str] = []
    for line in value.split("\n"):
        line = line.strip(BLANKS)
        match = KEY_LINE.fullmatch(line)
        if match:
            if match.group(1) != "LEGION" and match.group(1) not in keys:
                keys.append(match.group(1))
        elif line:
            terms.append(line)
    return keys, terms


def placeholder(key: str) -> str:
    digest = hashlib.sha256(SALT + key.encode()).digest()
    return "".join(chr(ord("A") + byte % 26) for byte in digest[: len(key)])


def placeholders(keys: list[str], terms: list[str]) -> dict[bytes, bytes]:
    table: dict[bytes, bytes] = {}
    seen: set[str] = set()
    for key in keys:
        value = placeholder(key)
        if value in seen or value in keys or value == "LEGION":
            refuse("two project keys of the list share a placeholder, or a placeholder is a key; "
                   f"raise the version in SALT ({SALT.decode()!r}) and run again over the whole tree")
        if any(term.casefold() in value.casefold() for term in terms):
            refuse("a placeholder holds a term of the list; raise the version in SALT and run again")
        seen.add(value)
        table[key.encode()] = value.encode()
    return table


# The words of a Legion role name that follow its project (legion-K-controller, K-architect).
ROLE_WORDS = rb"(?:controller|architect|implementer|reviewer|tester|planner|lead|fan|daemon|wake)"


def rewriter(table: dict[bytes, bytes], words: bool | str = False):
    # Legion writes a project's token in lower case (claim.ProjectToken) and reads its project back
    # as the token's upper case, so a token takes the placeholder's lower case.
    lower = {key.lower(): value.lower() for key, value in table.items()}
    table = {**table, **lower}

    def group(keys) -> bytes:
        # Longest first, so a key that starts another key's name is tried after it.
        return rb"(?P<k>" + b"|".join(re.escape(key) for key in sorted(keys, key=len, reverse=True)) + rb")"

    k, kl = group(set(table) - set(lower)), group(lower)
    # A lower-case key, or its placeholder, as the issue a role's name carries after its token.
    any_lower = rb"(?:" + b"|".join(re.escape(key) for key in sorted(set(lower) | set(lower.values()), key=len, reverse=True)) + rb")"
    alone = rb"(?![A-Za-z0-9_])"
    after = rb"(?<![A-Za-z0-9_$])"
    forms = [
        # An issue key, and one right after an escape sequence in a string ("\nK-12"), where no
        # word boundary falls.
        rb"(?:\b|(?<=\\[nrt]))" + k + rb"(?=-[0-9]+\b)",
        # The project of a Dispatch link.
        rb"(?i:" + re.escape(SCHEME) + rb")" + k + alone,
        # A quoted project key.
        rb"(?P<q>[\"'`])" + k + rb"(?=(?P=q))",
        # A token of a subject.
        rb"\bnotifications(?:\.[A-Za-z0-9_*>${}<-]+)*?\." + k + alone,
        # The project of a role.
        after + k + rb"(?=\.(?:legion-|<|[\"'`]|" + ROLE_WORDS + rb"\b))",
        rb"\blegion-" + k + rb"(?=-)",
        after + k + rb"(?=[- ]" + ROLE_WORDS + rb"s?\b)",
        # A Legion role's token, in lower case, and the issue its name carries after it
        # (legion-k-controller, legion-k-1000-architect, legion-k-k-12-architect, and the token
        # alone before a line's end or a quote): Legion reads both back in upper case. Any other
        # name that starts so (a release archive's legion-k-amd64, a repository's legion-k-<id>)
        # is no role and is left.
        rb"\blegion-" + kl + rb"(?=-(?:(?:[0-9]+-)?" + ROLE_WORDS + rb"\b|" + any_lower + rb"-[0-9]+\b|\\|[\s\"'`]|$))",
        rb"\blegion-[a-z0-9]+-" + kl + rb"(?=-[0-9]+\b)",
        # A key a program builds or escapes (a JSON text escapes the escape).
        after + k + rb"(?=-(?:[%${\"'`\\ ]|\r?$)|\\{1,2}-[0-9])",
        # A search form.
        after + k + rb"(?=(?: |:)[0-9]+\b)",
        # A project after the words Dispatch and Legion write it after.
        rb"\b(?:project|daemon):" + k + alone,
        rb"(?:/|\\/)projects(?:/|\\/)" + k + alone,
        rb"project=" + k + alone,
        rb"\b[Pp]roject " + k + alone,
    ]
    if words:
        # --words: every word that is a key, and every lower-case issue slug (k-12), for a file a
        # person has read and found holds no other word of those letters; --lower-words also every
        # lower-case word that is a key, for a file where each such word is the project.
        forms.append(after + k + alone)
        forms.append(after + kl + (alone if words == "lower" else rb"(?=-[0-9]+\b)"))
    patterns = [re.compile(form, re.MULTILINE) for form in forms]
    anywhere = re.compile(b"|".join(re.escape(key) for key in table))

    def replace(match: re.Match) -> bytes:
        start = match.start("k") - match.start()
        end = match.end("k") - match.start()
        whole = match.group(0)
        return whole[:start] + table[match.group("k")] + whole[end:]

    def rewrite(text: bytes) -> bytes:
        # Until nothing changes: a subject that names a project twice is read again from the start.
        while anywhere.search(text):
            new = text
            for pattern in patterns:
                new = pattern.sub(replace, new)
            if new == text:
                return text
            text = new
        return text

    return rewrite


def git(*args: str) -> bytes:
    try:
        return subprocess.run(["git", *args], check=True, capture_output=True).stdout
    except (OSError, subprocess.CalledProcessError) as error:
        refuse(f"git {args[0]} failed: {error}")
        raise


def listed_paths(targets: list[str]) -> list[str]:
    if not targets:
        out = git("-c", "core.quotePath=false", "ls-files", "-z", "--cached", "--others",
                  "--exclude-standard")
        return sorted({p.decode("utf-8", "surrogateescape") for p in out.split(b"\0") if p})
    paths: set[str] = set()
    for target in targets:
        if not os.path.lexists(target):
            refuse(f"{target} does not exist")
        if os.path.isdir(target) and not os.path.islink(target):
            for root, dirs, files in os.walk(target):
                dirs.sort()
                paths.update(os.path.join(root, name) for name in files)
        else:
            paths.add(target)
    return sorted(paths)


def text_file(path: str) -> bool:
    return (os.path.isfile(path) and not os.path.islink(path)
            and os.path.basename(path) not in LOCKFILES)


def tracked(path: str) -> bool:
    return subprocess.run(["git", "ls-files", "--error-unmatch", "--", path],
                          capture_output=True).returncode == 0


def main(argv: list[str]) -> int:
    check = words = False
    targets: list[str] = []
    options = True
    for arg in argv:
        if options and arg == "--":
            options = False
        elif options and arg == "--check":
            check = True
        elif options and arg == "--words":
            words = words or True
        elif options and arg == "--lower-words":
            words = "lower"
        elif options and arg in ("-h", "--help"):
            print(__doc__)
            return 0
        elif options and arg.startswith("-"):
            refuse(f"unknown option {arg} (a path that starts with - goes after --)")
        else:
            targets.append(arg)
    if words and not targets:
        refuse("--words and --lower-words read only the files they are given, each read by a "
               "person first: name them")

    keys, terms = read_list(os.environ.get("PRIVATE_NAME_TERMS", ""))
    if not keys:
        refuse("PRIVATE_NAME_TERMS holds no key: line, so there is no project key to replace "
               "and nothing was read")
    rewrite = rewriter(placeholders(keys, terms), words)

    if not targets:
        os.chdir(git("rev-parse", "--show-toplevel").decode().rstrip("\n"))
    inside = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True)
    top = os.path.realpath(inside.stdout.decode().rstrip("\n")) if inside.returncode == 0 else ""

    def kept(path: str) -> bool:
        if not top:
            return False
        relative = os.path.relpath(os.path.realpath(path), top)
        return any(relative == keep or (keep.endswith("/") and relative.startswith(keep))
                   for keep in KEEP_PATHS)

    paths = listed_paths(targets)
    # The renames first, each checked before anything is written, so a refusal leaves the tree as
    # it was.
    renames: list[tuple[str, str]] = []
    for path in paths:
        if not os.path.lexists(path) or kept(path):
            continue
        new_path = rewrite(os.fsencode(path)).decode("utf-8", "surrogateescape")
        if new_path != path:
            renames.append((path, new_path))
    if not check:
        targets_seen: set[str] = set()
        for _, new_path in renames:
            if os.path.lexists(new_path) or new_path in targets_seen:
                refuse(f"{new_path} exists, so a path that names a key cannot be renamed to it")
            targets_seen.add(new_path)

    changed_files = changed_lines = 0
    for path in paths:
        if not text_file(path) or kept(path):
            continue
        data = Path(path).read_bytes()
        if b"\0" in data:
            continue
        new = rewrite(data)
        if new == data:
            continue
        old_lines, new_lines = data.split(b"\n"), new.split(b"\n")
        lines = [i + 1 for i, (a, b) in enumerate(zip(old_lines, new_lines)) if a != b]
        changed_files += 1
        changed_lines += len(lines)
        shown = rewrite(os.fsencode(path)).decode("utf-8", "surrogateescape")
        if check:
            for line in lines:
                print(f"{shown}:{line}")
        else:
            Path(path).write_bytes(new)

    renamed = len(renames)
    for path, new_path in renames:
        if check:
            print(f"{new_path}: its path")
            continue
        os.makedirs(os.path.dirname(new_path) or ".", exist_ok=True)
        if tracked(path):
            git("mv", "--", path, new_path)
        else:
            os.rename(path, new_path)
        parent = os.path.dirname(path)
        while parent and os.path.isdir(parent) and not os.listdir(parent):
            os.rmdir(parent)
            parent = os.path.dirname(parent)

    count = f"{len(keys)} project key{'s' if len(keys) != 1 else ''} read"
    summary = (f"{changed_lines} line{'s' if changed_lines != 1 else ''} in {changed_files} "
               f"file{'s' if changed_files != 1 else ''}, {renamed} path{'s' if renamed != 1 else ''}")
    if check:
        print(f"neutralize-private-keys: {count}; "
              + (f"{summary} name a project key" if changed_lines or renamed else "no text or path names a project key"))
        return 1 if changed_lines or renamed else 0
    print(f"neutralize-private-keys: {count}; rewrote {summary}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
