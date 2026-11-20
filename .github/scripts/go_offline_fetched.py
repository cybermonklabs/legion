"""Rule 7 of check-go-offline.sh: each go command a step of a required Go job runs in its own run
text loads only packages a fetch before it, in the same job, names.

A required Go job runs with GOPROXY=off (rule 4), so its go steps read the module cache its restore
and its fetch step filled, and nothing else. The fetch step runs the job's fetch file
(.github/go-fetch/<job>.txt), and the fill of the job's cache family runs the same file (rule 22 of
check-ci-filters.sh). A go step whose packages no line of that file covers reads modules only an
older entry restored by its key's prefix happens to hold: it passes there, and fails, naming the
module, on the run that restores the entry its own fill saved, which may be another change's. This
holds each such step to its fetch before CI runs it.

Which go commands: each one the step's run text runs, and each one a shell that text hands more
shell to runs (the string after bash -c, what eval joins, a heredoc a shell reads, the echo, printf
or cat a pipe feeds a shell), as rule 1 reads them (go_offline_sites.shell_scope and the
source_shell readers).

What a go command needs: the packages it names (a pattern ending in /... is every package under its
directory in the same Go module, as go reads it; no package is the directory itself, and a .go file
is its directory), in the directory it runs in (the step's working-directory, else the job's or the
workflow's defaults.run, then each `cd` of the step before it, one inside a subshell for that
subshell alone, then its -C, before its subcommand or as its first flag, -C DIR or -C=DIR), with its
tests when it is go test or go vet, under its -tags (-tags T or -tags=T; else the -tags=T of its
GOFLAGS) and its GOOS, GOARCH, CGO_ENABLED and GOWORK. Its environment is the step's, the job's and
the workflow's env, then each export or unset before it (in its own subshell or function, or above
them), then its own NAME=value prefix and an `env NAME=value` before the go; a value that is a
`${{ }}` expression is not literal. The subcommands that load packages: build, install, run (its
first package alone), list, test and vet. go_offline_argv.py reads a go command's words and a
fetch's for this rule and for rule 2 alike.

What a fetch names: each `[GOOS=..] [GOARCH=..] [CGO_ENABLED=..] [GOWORK=off] deps DIR [-test]
[-tags T] PACKAGE...` line of each `go-fetch.sh file FILE` a command before it in the job runs (DIR
and FILE read from the repository's root, as go-fetch.sh reads them), and each `go-fetch.sh deps DIR
...` such a command runs (DIR read from its own directory, under its command's environment).

A command is held when one such line names a pattern holding each of its packages, with -test where
the command needs its tests, the same set of tags and the same four settings. A command no line
holds is refused at its workflow, naming the job, the step, the command and the deps line that would
fetch it, unless it says why it needs none: a `go-offline: <why>` comment on its own line of the run
text or within the 5 lines above it, or one its step carries as rule 2 reads it (in its run text,
or in a comment within the 5 lines above its name). A command whose packages, tags, directory or
settings are not literal ($pkg, "$flag", GOARCH=$arch, cd "$dir", a GOFLAGS that is not), or that
names an import path rather than a path from its directory, or is go list -m (which reads the
module graph, not packages), is refused too, unless such a comment (on its line, above it, or above
its step's name) says which line of the fetch file covers it: no fetch can be held to what this
cannot read. A command whose own GOPROXY (from the environment above) is not off goes online on
purpose, which rule 4 holds; it is not read here.

Its limit: a go command inside a script file or a shell function the step runs is not read (rule 2
holds an offline build there, and rule 1 each go command of the script); the job env's GOPROXY=off
fails such a command at run time, naming the module its cache lacks.

Imported with PYTHONPATH naming the directory this file lives in. Its test is
check-go-offline.test.sh, with the cases it sources.
"""
import os
import posixpath
from pathlib import Path
from typing import NamedTuple

import yaml

import source_scan as scan
from go_offline_argv import SETTINGS, UNREAD, deps_args, fetch_calls, fetch_file_calls, go_args, go_settings, goflags_tags
from go_offline_sites import REASON, go_subcommand, goish_text, in_scope, shell_scope
from go_offline_walk import step_reasoned
from source_shell import shell_command_word, shell_env_elements, shell_inner_scripts, shell_script, shell_stdin_scripts

LOADS = {"build", "install", "run", "list", "test", "vet"}
# What a go command's environment is read for: its platform and workspace (SETTINGS), its GOFLAGS,
# whose -tags go reads where the command gives none, and its GOPROXY: a command whose GOPROXY is
# not off goes online on purpose, which is rule 4's to hold.
NAMES = (*SETTINGS, "GOFLAGS", "GOPROXY")
# How deep a shell handed to a shell is read, as go_offline_sites.shell_sites reads it.
DEPTH = 3


class Load(NamedTuple):
    """What a go command loads, or a deps line fetches: its packages as repository paths (a pattern
    ends in /...), whether with their tests, its tags, and its four settings (None where unset)."""
    packages: tuple
    tests: bool
    tags: frozenset
    settings: tuple


def module_root(directory):
    """The nearest directory at or above `directory` that holds a go.mod, or None."""
    here = directory
    while True:
        if os.path.isfile(os.path.join(here or ".", "go.mod")):
            return here
        if here in ("", "."):
            return None
        here = posixpath.dirname(here)


def place(directory, arg):
    """A package argument as a repository path: '.' for the root; a /... pattern kept as one."""
    wild = arg == "..." or arg.endswith("/...")
    base = arg[:-3] if wild else arg
    if base.endswith(".go"):
        base = posixpath.dirname(base) or "."
    path = posixpath.normpath(posixpath.join(directory, base or "."))
    return f"{path}/..." if wild else path


def holds(pattern, package):
    """Whether a fetched pattern holds a needed package (or package pattern): the same path, or a
    path under a /... pattern's directory in the same Go module, since go's /... ends at a nested
    module."""
    if pattern == package:
        return True
    if not pattern.endswith("/..."):
        return False
    top = pattern[:-4]
    path = package[:-4] if package.endswith("/...") else package
    if not (path == top or top == "." or path.startswith(top + "/")):
        return False
    return module_root(path) == module_root(top)


def relative(pattern, module):
    """A repository path (or /... pattern) as a deps line names it, from its module's directory."""
    wild = pattern.endswith("/...")
    rel = posixpath.relpath(pattern.removesuffix("/..."), module)
    shown = "." if rel == "." else "./" + rel
    return f"{shown}/..." if wild else shown


def covered(need, lines):
    return any(line.tests >= need.tests and line.tags == need.tags and line.settings == need.settings
               and all(any(holds(p, n) for p in line.packages) for n in need.packages)
               for line in lines)


def tag_set(value):
    return frozenset(t for t in value.replace(" ", ",").split(",") if t)


def literal(value):
    """A setting's value, None where unset, UNREAD where it is not literal ($x, `${{ }}`)."""
    if value is None:
        return None
    value = str(value)
    return UNREAD if UNREAD in value or "${{" in value else value


def settings_of(values):
    """The four SETTINGS of a mapping of name -> value (go_settings's, or a fetch line's prefix)."""
    return tuple(literal(values.get(name)) for name in SETTINGS)


def deps_line(directory, words, settings):
    """A Load of a deps line's words after `deps DIR`, or None when one is not literal."""
    args = deps_args(words)
    if not args.literal:
        return None
    tags = tag_set(args.tags) if args.tags else frozenset()
    return Load(tuple(place(directory, p) for p in args.packages), args.tests, tags, settings)


def fetch_file(path):
    """The Loads of each deps line of a fetch file (go-fetch.sh's file mode), [] when it is absent."""
    if not os.path.isfile(path):
        return []
    lines = []
    for call in fetch_file_calls(Path(path).read_text()):
        if call.mode == "deps" and len(call.words) >= 2:
            load = deps_line(posixpath.normpath(call.words[0]), call.words[1:], settings_of(call.settings))
            if load is not None:
                lines.append(load)
    return lines


def need_of(go, directory, seen):
    """(the Load a go command needs, from its go_args; None for a subcommand that loads nothing; or
    UNREAD when what it loads is not literal). directory: where it runs before its -C; seen: its
    environment's values (go_settings over NAMES)."""
    if go.sub not in LOADS:
        return None
    if go.chdir is not None:
        directory = posixpath.normpath(posixpath.join(directory, go.chdir))
    if UNREAD in directory:
        return UNREAD
    if go.sub == "list" and (go.flag("-m") or ("-mod", "mod") in go.flags):
        return UNREAD
    tags = go.tags if go.tags is not None else goflags_tags(literal(seen.get("GOFLAGS")))
    if tags is not None and UNREAD in tags:
        return UNREAD
    packages = []
    for arg in go.operands:
        if UNREAD in arg or not (arg.startswith((".", "/")) or arg.endswith(".go")):
            return UNREAD
        packages.append(place(directory, arg))
    settings = settings_of(seen)
    if UNREAD in [s for s in settings if s is not None]:
        return UNREAD
    return Load(tuple(packages) or (directory,), go.sub in ("test", "vet"), tag_set(tags) if tags else frozenset(),
                settings)


def go_env(document, job, step):
    env = {}
    for scope in (document, job, step):
        env.update({str(k): v for k, v in ((scope or {}).get("env") or {}).items()})
    return env


def step_directory(document, job, step):
    default = ((document.get("defaults") or {}).get("run") or {}).get("working-directory", ".")
    job_dir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", default)
    directory = str(step.get("working-directory", job_dir))
    return UNREAD if "${{" in directory else posixpath.normpath(directory)


class Step:
    """One step's run text, read: the job's fetches so far (shared with the job's other steps,
    in order), the step's env, its run text's lines and whether it carries a reason; the commands
    it holds or that say why, and those no fetch holds."""

    def __init__(self, fetched, env, run, reasoned):
        self.fetched, self.env, self.lines, self.reasoned = fetched, env, run.split("\n"), reasoned
        self.held = self.said = 0
        self.unread, self.missing = [], []

    def read(self, text, line, directory, base, depth=0):
        """The commands of a shell text, in order: each cd, each go-fetch.sh and each go command,
        and each shell the text hands more shell to, read from where it is handed. base: the
        environment elements the command that handed this text over gave it."""
        commands = shell_script(text, line)
        _, _, exports = shell_scope(commands, NAMES)
        # The directory each command runs in, by the frames (subshells, $(...)) that hold it: a cd
        # changes its own frame's, and a subshell's ends with the subshell.
        directories = {(): directory}
        for order, command in enumerate(commands):
            values = command.values()
            start = shell_command_word(command)
            if start is None:
                continue
            frames = command.frames
            here = next(directories[frames[:n]] for n in range(len(frames), -1, -1) if frames[:n] in directories)
            scope = [*base, *in_scope(exports, order, frames)]
            if values[start] == "cd" and not command.forked:
                target = values[start + 1] if start + 1 < len(values) else ""
                unread = UNREAD in target + here or not target or target.startswith(("/", "~", "-"))
                directories[frames] = UNREAD if unread else posixpath.normpath(posixpath.join(here, target))
                continue
            for call in fetch_calls(values):
                if UNREAD in call.words[0] + here:
                    continue
                if call.mode == "file":
                    self.fetched += fetch_file(posixpath.normpath(posixpath.join(here, call.words[0])))
                elif call.mode == "deps":
                    seen = go_settings(self.env, [*scope, *shell_env_elements(command, start)], NAMES)
                    load = deps_line(posixpath.normpath(posixpath.join(here, call.words[0])), call.words[1:],
                                     settings_of(seen))
                    self.fetched += [load] if load is not None else []
            for k in range(len(values)):
                if goish_text(values[k]) and go_subcommand(values, k):
                    self.go_command(command, k, here, [*scope, *shell_env_elements(command, k)])
            if depth < DEPTH:
                own = shell_env_elements(command, len(command.words))
                for inner, inner_line in shell_inner_scripts(command) + shell_stdin_scripts(commands, order):
                    self.read(inner, inner_line, here, [*scope, *own], depth + 1)

    def go_command(self, command, k, directory, elements):
        seen = go_settings(self.env, elements, NAMES)
        # A command that sends its go online on purpose (its own GOPROXY) is rule 4's to hold.
        if seen.get("GOPROXY") != "off":
            return
        need = need_of(go_args(command.values(), k), directory, seen)
        if need is None:
            return
        shown = " ".join(w.source() for w in command.words[k:])[:200]
        if need != UNREAD and covered(need, self.fetched):
            self.held += 1
        elif self.reasoned or scan.reasoned(self.lines, "sh", REASON, [command.line]):
            self.said += 1
        elif need == UNREAD:
            self.unread.append(shown)
        else:
            self.missing.append((need, shown))


def check(workflows, errors):
    """Rule 7 over each required workflow (path -> text): each offline go command a step of a job
    whose env sets GOPROXY=off runs, that no fetch before it holds or whose packages, tags,
    directory or settings it cannot read, and that says no reason, adds an error. Returns
    (commands held, commands that say why)."""
    held = reasoned = 0
    for workflow_path in sorted(workflows):
        text = Path(workflow_path).read_text()
        document = yaml.safe_load(text) or {}
        raw = text.split("\n")
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, dict) or str(go_env(document, job, {}).get("GOPROXY")) != "off":
                continue
            fetched = []
            for step in job.get("steps") or []:
                run = step.get("run")
                if not isinstance(run, str):
                    continue
                name = step.get("name") or run.strip().splitlines()[0]
                where = f"job {job_id}, step \"{name}\""
                read = Step(fetched, go_env(document, job, step), run, step_reasoned(raw, step, run))
                read.read(run, 0, step_directory(document, job, step), [])
                held, reasoned = held + read.held, reasoned + read.said
                for shown in read.unread:
                    errors.append((workflow_path, 1, f"{where}, runs `{shown}`, whose packages, tags, directory "
                                   f"or GOOS, GOARCH, CGO_ENABLED or GOWORK this rule cannot read, so no fetch "
                                   f"can be held to it: write them literally, or say in a `# go-offline: <why>` "
                                   f"line above it, or above its step's name, which deps line of the job's "
                                   f"fetch file covers it"))
                for need, shown in read.missing:
                    settings = " ".join(f"{n}={v}" for n, v in zip(SETTINGS, need.settings) if v is not None)
                    module = module_root(need.packages[0].removesuffix("/...")) or "."
                    wanted = " ".join(relative(p, module) for p in need.packages)
                    flags = (" -test" if need.tests else "") + (f" -tags {','.join(sorted(need.tags))}"
                                                                 if need.tags else "")
                    errors.append((workflow_path, 1, f"{where}, runs `{shown}`, which loads "
                                   f"{' '.join(need.packages)}, and no fetch before it in the job names them: add "
                                   f"`{(settings + ' ') if settings else ''}deps {module}{flags} {wanted}` to the "
                                   f"job's fetch file under .github/go-fetch/, or say in a `# go-offline: <why>` line "
                                   f"above it why it needs none"))
    return held, reasoned
