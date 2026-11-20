"""The one reading of a go command's words and of a go-fetch.sh command's words, which rule 2
(go_offline_walk.py) and rule 7 (go_offline_fetched.py) of check-go-offline.sh both import: where a
go command runs (its -C), its subcommand, its flags with their values, its -tags and the words that
name what it loads; the settings it reads from its environment; and the mode and words of each
go-fetch.sh command, in a step or as a line of a fetch file. Each rule keeps its own policy over
what this reads: rule 2 holds an offline build to a fetch of its module's packages, rule 7 holds
each go command to a deps line by its packages, tests, tags and platform.

A word no reading can know is held as UNREAD (source_shell's placeholder for an expansion) or None,
as the caller gives it, and is passed on as it is: each rule decides what such a word means.

Imported with PYTHONPATH naming the directory this file lives in. Its test is
check-go-offline.test.sh, with the cases it sources.
"""
from typing import NamedTuple

from source_scan import CLEAR

UNREAD = "\x00"
# A go flag whose next word is its value, not a package.
FLAGS_WITH_VALUES = {"-o", "-C", "-ldflags", "-gcflags", "-asmflags", "-tags", "-run", "-skip", "-timeout", "-count",
                     "-p", "-mod", "-modfile", "-exec", "-bench", "-benchtime", "-coverprofile", "-cpuprofile",
                     "-memprofile", "-cpu", "-parallel", "-f", "-overlay", "-pkgdir", "-toolexec", "-covermode",
                     "-coverpkg", "-outputdir", "-buildmode", "-installsuffix", "-fuzz", "-fuzztime",
                     "-fuzzminimizetime", "-shuffle", "-vet", "-blockprofile", "-mutexprofile", "-trace", "-pgo"}
# The settings a deps line may name in front of it, which choose the files a package's imports are
# read from (GOOS, GOARCH, CGO_ENABLED) or leave the workspace (GOWORK=off).
SETTINGS = ("GOOS", "GOARCH", "CGO_ENABLED", "GOWORK")


class GoArgs(NamedTuple):
    """A go command's words after the go. chdir: the directory its -C names, as written (None with
    no -C). sub: its subcommand. flags: each flag as (name, value), the value its `=` gives or, for a
    flag of FLAGS_WITH_VALUES, the next word (None for a flag that takes none). tags: its -tags
    value as written, the last one (None with none). operands: the words after its flags that name
    what it loads (packages, .go files, path@version): for go run the first alone, since the rest
    are the program's; none after go test's -args."""
    chdir: object
    sub: object
    flags: tuple
    tags: object
    operands: tuple

    def flag(self, name):
        """Whether the command carries the flag `name` (-m, -c, -list)."""
        return any(n == name for n, _ in self.flags)


def _chdir(words):
    """(a -C's directory, the words after it) when words starts with -C DIR or -C=DIR, else
    (None, words)."""
    if words[:1] == ["-C"] and len(words) > 1:
        return words[1], words[2:]
    if words and words[0] is not None and words[0].startswith("-C="):
        return words[0][3:], words[1:]
    return None, words


def go_args(values, k):
    """The GoArgs of the go command whose go is word k of values (a word no reading knows is
    UNREAD in it, or None). go reads its -C before the subcommand or as the subcommand's first flag,
    and so does this. None when no subcommand follows."""
    chdir, rest = _chdir(list(values[k + 1:]))
    if not rest:
        return None
    sub, args = rest[0], rest[1:]
    if chdir is None:
        chdir, args = _chdir(args)
    flags, tags, operands, takes = [], None, [], None
    for word in args:
        if takes is not None:
            flags.append((takes, word))
            tags = word if takes == "-tags" else tags
            takes = None
            continue
        if word is None or word == "-" or not word.startswith("-"):
            operands.append(word)
            if sub == "run":
                break
            continue
        if word == "-args":
            break
        name, eq, value = word.partition("=")
        if eq:
            flags.append((name, value))
            tags = value if name == "-tags" else tags
        elif word in FLAGS_WITH_VALUES:
            takes = word
        else:
            flags.append((word, None))
    return GoArgs(chdir, sub, tuple(flags), tags, tuple(operands))


def goflags_tags(value):
    """The -tags a GOFLAGS value gives go: its last -tags=T (GOFLAGS holds -flag=value words);
    None with none; UNREAD when the value is not literal."""
    if value is None:
        return None
    if UNREAD in value:
        return UNREAD
    tags = None
    for word in value.split():
        name, eq, given = word.partition("=")
        if name in ("-tags", "--tags") and eq:
            tags = given
    return tags


def go_settings(base, elements, names):
    """The value each of `names` has for a go command: base (a mapping: the workflow's, job's and
    step's env, merged) and then each environment element in the order the command's shell makes
    them (("set", NAME, value) with value None where it is not literal, or CLEAR, which empties the
    environment, base included). A name nothing sets is absent; one an unreadable element sets is
    UNREAD; another element the shell's readers give (the inherited environment, one they cannot
    read) leaves every name it may set UNREAD."""
    out = {name: str(base[name]) for name in names if name in base and base[name] is not None}
    for element in elements:
        if element == CLEAR:
            out = {}
        elif element[0] == "set" and element[1] in names:
            if element[2] is None:
                out[element[1]] = UNREAD
            else:
                out[element[1]] = element[2]
        elif element[0] != "set":
            out = {name: UNREAD for name in names}
    return out


class FetchCall(NamedTuple):
    """One go-fetch.sh command: the settings in front of it (a fetch file line's GOOS=... prefix;
    none for a call in a step, whose settings are its command's environment), its mode (file, deps,
    tools, tidy, pulumi-consumer) and the words after the mode, the first of them its directory or
    its file."""
    settings: dict
    mode: object
    words: tuple


def fetch_file_calls(text):
    """Each command line of a fetch file's text as a FetchCall; a comment, from #, and a blank line
    are none."""
    calls = []
    for raw in text.splitlines():
        words = raw.split("#", 1)[0].split()
        settings = {}
        while words and "=" in words[0]:
            name, _, value = words.pop(0).partition("=")
            settings[name] = value
        if words:
            calls.append(FetchCall(settings, words[0], tuple(words[1:])))
    return calls


def fetch_calls(values):
    """Each go-fetch.sh command in an argv (values; a word no reading knows is UNREAD in it, or
    None) that names a mode and an operand, as a FetchCall."""
    return [FetchCall({}, values[k + 1], tuple(values[k + 2:])) for k, value in enumerate(values)
            if value and value.endswith("go-fetch.sh") and k + 2 < len(values)]


class DepsArgs(NamedTuple):
    """A deps command's words after its directory: whether it lists the packages' tests (-test),
    its -tags value as written (None with none), its packages, and whether every one of those words
    is literal."""
    tests: bool
    tags: object
    packages: tuple
    literal: bool


def deps_args(words):
    """The DepsArgs of a deps command's words after `deps DIR`. A flag of FLAGS_WITH_VALUES takes
    the next word, -tags among them. A word no reading knows (None) or a shell operator (|, ;, &)
    ends a call's words; a word with UNREAD in it is kept, and marks the words not literal."""
    tests, tags, packages, literal, takes = False, None, [], True, None
    for word in words:
        if word is None or word.startswith(("|", ";", "&")):
            break
        literal &= UNREAD not in word
        if takes is not None:
            tags = word if takes == "-tags" else tags
            takes = None
        elif word == "-test":
            tests = True
        elif word.startswith("-tags="):
            tags = word[len("-tags="):]
        elif word.startswith("-"):
            takes = word if word in FLAGS_WITH_VALUES else None
        else:
            packages.append(word)
    return DepsArgs(tests, tags, tuple(packages), literal)
