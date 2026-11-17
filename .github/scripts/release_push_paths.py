"""The push-path rule of check-legion-version-inputs.sh, whose header states it as part of rule 3:
each path a release workflow's version step reads is covered whole by a pattern of the workflow's
on.push.paths, and no later `!` pattern can drop a change to it. A commit that touches only a path
no pattern covers runs no release workflow, so nothing releases until a later commit does, and no
release step changes.

  problems(path, patterns, key, required, excludes)
      the rule's refusals for the workflow `path`, whose on.push.paths is the list of strings
      `patterns`: each message, in the order the check reports them.

It reads GitHub's patterns through workflow_paths.py's glob_regex and unread_patterns. Imported
from this directory, which the check puts on PYTHONPATH; the check's harness links it into each
fixture beside the script under test.
"""

import re

from workflow_paths import glob_regex, unread_patterns

# What a pattern holds beyond its literal part: `*`, `?`, a `[` class, a `\` escape.
WILDCARD = re.compile(r"[*?\[\\]")


def whole_cover(pattern, path):
    """Whether the push pattern `pattern`, not `!`-negated, covers every change to `path`, a file or
    a directory written with its /: `**`, the file itself, or `X/**` with X the directory itself or
    one above the path. GitHub's `**` keeps the `/` before it, so `X/**` covers what lies inside a
    directory X and never a file X. Any other glob is not read as covering anything."""
    if pattern == "**":
        return True
    if pattern.endswith("/**") and not WILDCARD.search(pattern[:-3]):
        return path.startswith(pattern[:-2])
    return not path.endswith("/") and pattern == path


def reaches(pattern, path):
    """Whether the `!`-negated push pattern `pattern` (its `!` taken off) can drop a change to
    `path`: for a file, the glob matches it; for a directory, the glob's literal part, up to its
    first wildcard, lies inside the directory or above it, so it may match something there."""
    if not path.endswith("/"):
        return bool(glob_regex(pattern).fullmatch(path))
    static = WILDCARD.split(pattern, maxsplit=1)[0]
    return static.startswith(path) or path.startswith(static)


def problems(path, patterns, key, required, excludes):
    """The refusals for the workflow `path`: a pattern GitHub reads another way than glob_regex, or
    one with a form GitHub's docs give no meaning; and each path its version step reads through
    `key`, `required`, that no pattern without `!` covers whole, or that a later `!` pattern can
    drop a change to, unless that pattern's literal part, up to its first wildcard, lies inside the
    directory an exclude of `key`'s paths drops (`!packages/envoy/infra/**` beside
    `:(exclude)packages/envoy/infra`; `!packages/envoy/infra*/**` also reaches a sibling such as
    infra2, so it is refused). `excludes` holds each exclude as (its text, the path it drops)."""
    found = []
    for pattern in unread_patterns(patterns):
        found.append(f"{path}'s on.push.paths pattern `{pattern}` uses `?`, `+`, `\\`, `{{` or `}}`, which GitHub "
                     f"reads another way than this check's glob does, so the check cannot say which changes it covers "
                     f"or drops; write it without them")
    for pattern in patterns:
        body = pattern.removeprefix("!")
        parts = body.split("/")
        forms = (["a `.` or `..` segment"] if any(part in (".", "..") for part in parts) else []) + (
            ["an empty segment (a leading `/`, a `//` or a trailing `/`)"] if "" in parts else []) + (
            ["a `[!` class"] if "[!" in body else [])
        if forms:
            found.append(f"{path}'s on.push.paths pattern `{pattern}` holds {' and '.join(forms)}, which GitHub's "
                         f"filter docs give no meaning, and matchers differ on it, so the check cannot say which "
                         f"changes it covers or drops; write the path with plain segments, and a class as `[...]`")
    dropped = [exclude.rstrip("/") + "/" for _, exclude in excludes]
    why = ("a commit that touches it alone runs no release workflow, so nothing releases until a later commit "
           "does, and no release step changes")
    for need in required:
        last = max((i for i, p in enumerate(patterns) if not p.startswith("!") and whole_cover(p, need)), default=None)
        if last is None:
            found.append(f"{path}'s on.push.paths runs the workflow for no change to {need}, which its version step "
                         f"reads through {key}: {why}; add `{need.rstrip('/') + '/**' if need.endswith('/') else need}` "
                         f"or a `X/**` above it")
            continue
        for pattern in patterns[last + 1:]:
            if not pattern.startswith("!") or not reaches(pattern[1:], need):
                continue
            static = WILDCARD.split(pattern[1:], maxsplit=1)[0]
            if any(static.startswith(d) for d in dropped):
                continue
            found.append(f"{path}'s on.push.paths pattern `{pattern}`, after `{patterns[last]}`, can leave out a "
                         f"change to {need}, which its version step reads through {key}: {why}")
    return found
