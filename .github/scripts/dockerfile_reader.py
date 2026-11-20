"""The one reader of a Dockerfile's syntax this repository has: its instructions, each continued
line joined, comments dropped and a RUN's here-document body given as its script. Two checks read
Dockerfiles through it: check-ci-filters.sh's rule 22 (.github/scripts/ci_network_installs.py), for
the installs and pulls a build makes, and check-image-trigger-paths.sh, for the files a build
reads; each interprets the instructions itself.
"""

import re


def instructions(text: str) -> list:
    """A Dockerfile's instructions as (line, KEYWORD, arguments): each continued line joined, a
    comment line skipped (inside a continuation too, as Docker drops it), and a RUN's here-document
    body (`RUN <<EOF` … `EOF`) given as its script."""
    lines = text.split("\n")
    out = []
    index = 0
    while index < len(lines):
        line = lines[index]
        start = index + 1
        index += 1
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        joined = line
        while joined.rstrip().endswith("\\") and index < len(lines):
            following = lines[index]
            index += 1
            if following.lstrip().startswith("#"):
                continue
            joined = joined.rstrip()[:-1] + " " + following
        keyword, _, arguments = joined.strip().partition(" ")
        heredoc = re.search(r"<<-?\s*['\"]?([A-Za-z_][A-Za-z0-9_]*)['\"]?", arguments) \
            if keyword.upper() == "RUN" else None
        if heredoc:
            body = []
            while index < len(lines) and lines[index].strip() != heredoc.group(1):
                body.append(lines[index])
                index += 1
            index += 1
            arguments = arguments[:heredoc.start()] + "\n".join(body)
        out.append((start, keyword.upper(), arguments.strip()))
    return out


def flags(arguments: str) -> tuple[list, str]:
    """An instruction's leading `--name=value` flags and the rest of its arguments."""
    found = []
    while arguments.startswith("--"):
        flag, _, arguments = arguments.partition(" ")
        found.append(flag)
        arguments = arguments.lstrip()
    return found, arguments


def substitute(text: str, values: dict) -> str:
    """A Dockerfile word with its `${NAME}` and `$NAME` read from the ARG defaults and build-args."""
    return re.sub(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", lambda m: values.get(m.group(1), m.group(0)), text)
