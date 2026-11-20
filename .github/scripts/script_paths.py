"""The one resolver of check-ci-filters.sh's rule 22: the path of this repository a command word
names, read from where the command runs (a Site), so the rule can read each shell script a step
runs and tell this repository's network-install.sh from any other script of that name.

_anchor reads a relative path (from the step's working directory, or a Dockerfile RUN's WORKDIR),
$GITHUB_WORKSPACE, $GITHUB_ACTION_PATH in a local action, $0 and the script's own directory however
written, a variable the text sets to one of those (environment()), and an image path a COPY or a
bind mount filled; resolved(), repository_script() and unfound_script() are what the policy
(.github/scripts/ci_network_installs.py) asks of it. The words come from
.github/scripts/shell_commands.py.
"""

import posixpath
import re
from dataclasses import dataclass, field

from ci_filter_coverage import REPO, read_repository_text
from shell_commands import STDIN_SHELLS, Scanner, literal_word, pipelines_of, substitution

# How a script's path or its directory is written, which the resolver reads as the script itself.
SCRIPT_PATH = {"$0", "${0}", "$BASH_SOURCE", "${BASH_SOURCE}", "${BASH_SOURCE[0]}"}
SCRIPT_DIRECTORY = {"${0%/*}", "${BASH_SOURCE%/*}", "${BASH_SOURCE[0]%/*}"}
# The variables that name a place outside the repository: a command there is no script of it.
OUTSIDE_VARIABLES = {"RUNNER_TEMP", "TMPDIR", "HOME", "RUNNER_TOOL_CACHE"}
# What the resolver gives a path outside the repository: known not to be a file of it.
OUTSIDE = "outside"
# The directories a Dockerfile's RUN finds a bare command in, which a COPY may fill.
IMAGE_PATH = ("/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")


@dataclass
class Site:
    """Where a command runs: the job and its workflow, the job's step it runs in, the file that
    holds the command (the workflow, a local action's action.yml, a repository script or a
    Dockerfile), the words that say how that file is reached from the job's step, the label of the
    step in that file, the directory a relative path is read from, a local action's directory
    ($GITHUB_ACTION_PATH), the repository script the text is ($0), the variables the text sets to a
    path the resolver establishes, and, for a Dockerfile's RUN, the stage's working directory and
    the image paths a COPY or a bind mount fills with a file of the repository."""
    path: str
    job_id: str
    step: str
    where: str
    via: str = ""
    label: str = ""
    base: str = ""
    action: str = ""
    script: str = ""
    env: dict = field(default_factory=dict)
    image: bool = False
    workdir: str = "/"
    files: dict = field(default_factory=dict)
    fakes: frozenset = frozenset()


def _normal(path: str) -> str:
    """An anchored path ("/" is the repository's root) with its `.` and `..` read, or OUTSIDE where
    a `..` climbs out of the root."""
    parts: list = []
    for part in path.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                return OUTSIDE
            parts.pop()
        else:
            parts.append(part)
    return "/" + "/".join(parts)


def _head(word: str) -> tuple[str, str]:
    """A word's leading expansion (`$(…)`, `${…}`, `$NAME`, `$0`) and the rest of it."""
    if word.startswith("$("):
        end = Scanner.balanced(word, 1)
        return word[:end + 1], word[end + 1:]
    if word.startswith("${"):
        end = word.find("}")
        return (word[:end + 1], word[end + 1:]) if end > 0 else (word, "")
    match = re.match(r"\$([A-Za-z_][A-Za-z0-9_]*|[0-9])", word)
    return (match.group(0), word[match.end():]) if match else ("", word)


def _image_file(path: str, site: Site) -> str:
    """The anchored repository path an image path holds, where a COPY or a bind mount put a file of
    the repository there, or OUTSIDE."""
    normal = posixpath.normpath(path)
    return "/" + site.files[normal] if normal in site.files else OUTSIDE


def _anchor(word: str, site: Site, depth: int = 0) -> str | None:
    """The path a command word names, the one resolver of rule 22: an anchored repository path ("/"
    is the root), OUTSIDE for a place known to be outside the repository (an absolute path, a
    scratch directory, $RUNNER_TEMP, a `..` past the root), or None where the word cannot be
    resolved. It reads, from the site, a relative path (from the step's working directory, or a
    RUN's WORKDIR), $GITHUB_WORKSPACE, $GITHUB_ACTION_PATH in a local action, $0 and the script's
    directory however written (`$(dirname "$0")`, `${BASH_SOURCE[0]%/*}`, `$(cd "$(dirname "$0")/.."
    && pwd)`, `$(realpath …)`, `$(git rev-parse --show-toplevel)`), a variable the text set to one
    of those, and an image path a COPY or a bind mount of the Dockerfile filled."""
    if depth > 8 or not word:
        return None
    head, rest = _head(word)
    if not head:
        if not literal_word(word.replace("[", "").replace("?", "")) or word.startswith(("<(", ">(", "`")):
            return None
        if word.startswith("/"):
            return _image_file(word, site) if site.image else OUTSIDE
        if site.image and "/" not in word:  # a bare command of a RUN: the image's PATH
            found = [f"{d}/{word}" for d in IMAGE_PATH if f"{d}/{word}" in site.files]
            return _image_file(found[0], site) if found else OUTSIDE
        if site.image:
            return _image_file(posixpath.join(site.workdir, word), site)
        return _normal(posixpath.join("/" + site.base, word))
    value = _expansion(head, site, depth)
    if value is None or value == OUTSIDE:
        return value
    if rest and (not rest.startswith("/") or not literal_word(rest)):
        return None
    return _normal(value + rest)


def _inner_texts(word: str) -> list:
    """The text of each substitution a word holds (`$(…)`, `<(…)`, `>(…)`), wherever it stands in it."""
    texts, index = [], 0
    while index < len(word):
        if word.startswith(("$(", "<(", ">("), index) and not word.startswith("$((", index):
            end = Scanner.balanced(word, index + 1)
            texts.append(word[index + 2:end])
            index = end + 1
        else:
            index += 1
    return texts


def _own_commands(text: str) -> list:
    """The commands a substitution's text runs itself, in order, leaving out those of the
    substitutions inside their words, which shell_commands also gives (`cd "$(dirname "$0")/.." &&
    pwd` runs cd and pwd)."""
    commands = [command for pipeline in pipelines_of(text) for command in pipeline]
    nested = [inner for command in commands for word in command for inner_text in _inner_texts(word)
              for pipeline in pipelines_of(inner_text) for inner in pipeline]
    return [command for command in commands if command not in nested]


def _expansion(head: str, site: Site, depth: int) -> str | None:
    """What one expansion at the start of a word names, for _anchor."""
    if head in SCRIPT_PATH:
        return "/" + site.script if site.script else None
    if head in SCRIPT_DIRECTORY:
        return "/" + posixpath.dirname(site.script) if site.script else None
    match = re.fullmatch(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", head)
    if match:
        name = match.group(1)
        if name == "GITHUB_WORKSPACE":
            return "/"
        if name == "GITHUB_ACTION_PATH" and site.action:
            return "/" + site.action
        if name == "PWD" and not site.image:
            return _normal("/" + site.base)
        if name in site.env:
            return site.env[name]
        return OUTSIDE if name in OUTSIDE_VARIABLES else None
    text = substitution(head) if head.startswith("$(") else None
    if text is None:
        return None
    commands = _own_commands(text)
    if len(commands) == 2 and commands[0][0] == "cd" and commands[1][0] == "pwd":
        target = [w for w in commands[0][1:] if w not in ("--", "-P", "-L")]
        return _anchor(target[0], site, depth + 1) if len(target) == 1 else None
    if len(commands) != 1:
        return None
    command = commands[0]
    arguments = [w for w in command[1:] if w != "--" and not w.startswith("-")]
    if command[0] == "dirname" and len(arguments) == 1:
        value = _anchor(arguments[0], site, depth + 1)
        return value if value in (None, OUTSIDE) else posixpath.dirname(value)
    if (command[0] == "realpath" or command[:2] == ["readlink", "-f"]) and len(arguments) == 1:
        return _anchor(arguments[0], site, depth + 1)
    if command[0] == "git" and "rev-parse" in command and "--show-toplevel" in command:
        return "/"
    if command[0] == "pwd" and not site.image:
        return _normal("/" + site.base)
    if command[0] == "mktemp":
        return OUTSIDE
    return None


def environment(assignments: list, site: Site) -> dict:
    """The variables a text sets to a path the resolver establishes, each from its assignments in
    order (each may read those before it). A variable assigned two different paths, or once to a
    value the resolver cannot read, is none of them."""
    env: dict = {}
    refused: set = set()
    for name, value in assignments:
        if name in refused:
            continue
        site.env = env
        anchored = _anchor(value, site) if value else None
        if anchored is None or env.get(name, anchored) != anchored:
            refused.add(name)
            env.pop(name, None)
        else:
            env[name] = anchored
    site.env = env
    return env


def resolved(word: str, site: Site) -> str | None:
    """The repository-relative path a command word names, or None where it names none (outside the
    repository, or past the resolver)."""
    anchored = _anchor(word, site)
    return None if anchored in (None, OUTSIDE) else anchored[1:]


def repository_script(word: str, site: Site) -> str | None:
    """The repository-relative path of the shell script a command word runs, where it names one
    this repository holds (a `.sh` file, or a file whose `#!` line names bash, sh, dash or zsh), read
    through the resolver from the site's directory, or, for a relative path, from the root; a bare
    name with no `/` is a program on PATH unless it ends in `.sh`, and in a Dockerfile's RUN it is
    the file a COPY put on the image's PATH. A word the resolver cannot place whose path after its
    first expansion names a shell script of the repository (`"$root/.github/scripts/x.sh"` in a
    test harness that runs its fixture's link to x.sh) is read as that script: reading it can only
    refuse more, and the wrapper itself is never placed this way (read_wrapped asks resolved)."""
    if "/" not in word:
        if site.image:
            candidates = [resolved(word, site)]
        elif not word.endswith(".sh"):
            return None
        else:
            candidates = [resolved(word, site), resolved(word, Site("", "", "", "", action=site.action))]
    else:
        candidates = [resolved(word, site)]
        head, rest = _head(word)
        if not head and not word.startswith("/") and not site.image:
            candidates.append(resolved(word, Site("", "", "", "", action=site.action)))
        if head and _anchor(word, site) is None and rest.startswith("/") and literal_word(rest):
            candidates.append(_normal(rest)[1:])
    for candidate in candidates:
        if candidate and (REPO / candidate).is_file():
            if candidate.endswith(".sh"):
                return candidate
            first = read_repository_text(REPO / candidate).split("\n", 1)[0]
            interpreter = first[2:].split() if first.startswith("#!") else []
            if interpreter[:1] and interpreter[0].endswith("/env"):
                interpreter = interpreter[1:]
            if interpreter and interpreter[0].rsplit("/", 1)[-1] in STDIN_SHELLS:
                return candidate
    return None


def unfound_script(word: str, site: Site) -> bool:
    """Whether a command word runs a shell script the resolver cannot place: a word ending in `.sh`
    that holds an expansion the resolver cannot read. A script it places outside the repository (a
    scratch directory's) is not one, and neither is one of the repository."""
    return word.rstrip('"\'').endswith(".sh") and _anchor(word, site) is None


