#!/usr/bin/env python3
"""No required job installs an Ubuntu package from the network while it runs.

daemon-go's `sudo apt-get update && sudo apt-get install tmux` hung 68 minutes in `apt-get update`
after the runner's azure mirror failed, and the job reached its 70-minute limit (run 44523167376,
attempt 2) on a change that touched nothing it tests. A required job installs each package from
the cache entry it restores instead (.github/scripts/apt-cache.sh, whose header says how), and this
rule refuses every other package install in a step of a required workflow: ci.yaml and every local
workflow it calls, followed through each called file, and the local actions their steps use,
followed through each action that uses another. A refusal names the workflow (or the action and the
jobs that use it), the job, the step and the command.

What is refused, in a step's `run:` text read as shell (below):
  * apt-get, apt or aptitude with any command but those that read the system and fetch nothing
    (list, show, policy, search, depends, rdepends, madison, showsrc, pkgnames, dump, dumpavail,
    stats, check, clean, autoclean, remove, purge, autoremove, autopurge, indextargets, help):
    `update`, `install`, `upgrade`, `download` and every command this list does not name;
  * dpkg installing a file (-i, --install, --unpack), which came from somewhere this rule cannot
    see, so it is no install from the cache;
  * Playwright's host packages: `playwright install --with-deps` and `playwright install-deps`,
    which run apt-get underneath;
  * apt-cache.sh `fill`, which always reads the network: it is ci-caches.yaml's, not a required
    job's;
  * apt-cache.sh `install` off its path: a step that has no `timeout-minutes`, or whose job (or
    action) has no actions/cache restore before it of the directory the install reads, keyed
    `apt-...` on the hash of the list the install reads and of apt-cache.sh itself. Without the
    restore every run is a miss, which fetches; without the bound a hung mirror is the job's limit
    again.

A decision, stated here as the rule's: a Playwright browser install with no --with-deps
(`bunx playwright install chromium`) passes when its step carries a `timeout-minutes`. It reads
Playwright's own download hosts and no apt mirror, Playwright makes up to five attempts across
those hosts within the step, and the step's bound ends a hang; a browser cache was weighed and left
out, because the three archives download in 8 to 9 s and an entry would add about 250 MiB to a
cache store GitHub evicts from (the reasons .github/workflows/dispatch-browser.yaml's
multiprocess job gives at its step of that name). One with no `timeout-minutes` is refused.

How a `run:` is read: by the shell reader the offline checks share (source_shell.py's
shell_script), as bash splits it into simple commands, a `$(...)` or backticks inside a word read
as commands of their own and a heredoc's body as no command. The script a shell is handed is read
the same way (source_shell.py's shell_inner_scripts and shell_stdin_scripts): the string after
its -c, what eval joins, a heredoc it reads with no script operand, and what an echo, a printf or
a cat of a heredoc pipes into it (`echo "apt-get install -y tmux" | sudo bash`). In front of the
command word, NAME=value settings and the wrappers sudo, env, timeout, nice, nohup, time,
command, exec, xargs and stdbuf are passed over, with their options: that list is this rule's,
and the reader is given it, so `sudo bash -c ...` hands its script on as `bash -c ...` does.

Its limits, each a form it reads as no install: a command word built at run time ("$tool" with tool
set to apt-get, eval of a variable), and a script a step runs (a repository script that runs
apt-get is not followed; apt-cache.sh is the one script here that does). It is a check of what the
steps say, as check-go-offline.sh is of its own.

The interface: check(root) gives (problems, summary), as go_offline_jobs.py's does: problems is a
(path, line, message) for each refusal, and summary one line of what was read, which starts
`UNKNOWN: ` when it read no required workflow or no run step of one. Run as a program, it reads the
tree it stands in (or the root it is given), prints each refusal as a GitHub error annotation, and
exits 1 on a refusal, 2 (UNKNOWN) when it read no required workflow or no step of one, and 0
otherwise. check-go-offline.sh calls check().

Run from anywhere: .github/scripts/package_installs.py [ROOT]
Its test is .github/scripts/package-installs.test.sh, in ci.yaml's test job.
"""
from __future__ import annotations

import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

import yaml

from source_shell import ShCommand, shell_inner_scripts, shell_script, shell_stdin_scripts

ROOT_WORKFLOW = ".github/workflows/ci.yaml"
CACHE_SCRIPT = ".github/scripts/apt-cache.sh"

APT_TOOLS = {"apt-get", "apt", "aptitude"}
# The apt commands that read the system or its indexes and fetch nothing.
APT_READS = {
    "list", "show", "policy", "search", "depends", "rdepends", "madison", "showsrc", "pkgnames",
    "dump", "dumpavail", "stats", "check", "clean", "autoclean", "remove", "purge", "autoremove",
    "autopurge", "indextargets", "help",
}
# The apt options that take the next word as their value.
APT_VALUE_OPTIONS = {"-o", "-c", "-t", "--option", "--config-file", "--target-release"}
PLAYWRIGHT = {"playwright", "playwright-core"}
# Wrappers: the command they run is a later word. Each maps to the options that take a value.
WRAPPERS = {
    "sudo": {"-u", "-g", "-p", "-C", "-D", "-h", "-r", "-t", "-U", "-T", "--user", "--group",
             "--prompt", "--chdir", "--host", "--role", "--type", "--other-user", "--command-timeout"},
    "env": {"-u", "-C", "-S", "--unset", "--chdir", "--split-string"},
    "timeout": {"-k", "-s", "--kill-after", "--signal"},
    "nice": {"-n", "--adjustment"},
    "nohup": set(),
    "time": {"-f", "-o", "--format", "--output"},
    "command": set(),
    "exec": {"-a"},
    "xargs": {"-a", "-d", "-E", "-I", "-L", "-n", "-P", "-s", "--arg-file", "--delimiter",
              "--max-args", "--max-lines", "--max-procs", "--max-chars"},
    "stdbuf": {"-i", "-o", "-e"},
}
ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=")
HASH_FILES = re.compile(r"hashFiles\(([^)]*)\)")
QUOTED = re.compile(r"'([^']*)'|\"([^\"]*)\"")


# --- Reading YAML with the line of each key, so a refusal names where to edit.
class _Loader(yaml.SafeLoader):
    pass


class Mapping(dict):
    """A YAML mapping that remembers the line of each key, and the line a block scalar's text
    starts on."""

    lines: dict
    text_lines: dict


def _mapping(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines, mapping.text_lines = {}, {}
    for key, value in node.value:
        if not hasattr(key, "value"):
            continue
        mapping.lines[key.value] = key.start_mark.line + 1
        start = value.start_mark.line + 1
        # A `|` or `>` scalar's text starts on the line after its indicator.
        if isinstance(value, yaml.ScalarNode) and value.style in ("|", ">"):
            start += 1
        mapping.text_lines[key.value] = start
    return mapping


_Loader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def load(path: Path):
    return yaml.load(path.read_text(), Loader=_Loader)


# --- Reading a run: text as shell, through the shared reader.
@dataclass
class Command:
    line: int  # 0-based, within the text
    words: list[str]  # each word with its quotes removed and its expansions as written (ShWord.source)


def program_index(words: list[str]) -> int | None:
    """The index of the command word: NAME=value settings and the wrappers, with their options,
    passed over; None when nothing is left."""
    k = 0
    while k < len(words):
        w = words[k]
        if ASSIGNMENT.match(w):
            k += 1
            continue
        name = Path(w).name
        if name in WRAPPERS:
            takes = WRAPPERS[name]
            k += 1
            while k < len(words) and (words[k].startswith("-") or (name in ("env", "sudo") and ASSIGNMENT.match(words[k]))):
                option = words[k].split("=", 1)[0]
                k += 2 if option in takes and "=" not in words[k] else 1
            if name == "timeout" and k < len(words):
                k += 1  # the duration
            if name == "nice" and k < len(words) and re.fullmatch(r"-?\d+", words[k]):
                k += 1
            continue
        return k
    return None


def _program(command: ShCommand) -> int | None:
    """program_index of a shared-reader command, the form source_shell.py's helpers take."""
    return program_index([w.source() for w in command.words])


def all_commands(text: str, line: int = 0) -> list[Command]:
    """Every simple command of the text, the commands of each $(...) and `...` included, and those
    of each script a shell is handed: the string after its -c, what eval joins, a heredoc it reads
    with no script operand, and what the command before it in a pipeline feeds it."""
    found = []
    commands = shell_script(text, line)
    for index, command in enumerate(commands):
        found.append(Command(command.line, [w.source() for w in command.words]))
        for inner, inner_line in shell_inner_scripts(command, _program) + shell_stdin_scripts(commands, index, _program):
            found.extend(all_commands(inner, inner_line))
    return found


# --- What a command is.
def apt_command(program: list[str]) -> str | None:
    """apt's command word (`install`), or "" for none, when the program is apt-get, apt or
    aptitude; None for any other program."""
    if Path(program[0]).name not in APT_TOOLS:
        return None
    k = 1
    while k < len(program):
        w = program[k]
        if w in APT_VALUE_OPTIONS:
            k += 2
            continue
        if w.startswith("-"):
            k += 1
            continue
        return w
    return ""


def installs_a_file(program: list[str]) -> bool:
    if Path(program[0]).name != "dpkg":
        return False
    return any(w in ("--install", "--unpack") or re.fullmatch(r"-[a-zA-Z]*i[a-zA-Z]*", w) for w in program[1:])


def playwright_install(program: list[str]) -> str | None:
    """'deps' for `playwright install --with-deps` or `playwright install-deps`, 'browsers' for a
    `playwright install` without --with-deps, None for anything else. The playwright word may sit
    after a runner (bunx, npx, bun x, pnpm exec) and carry a version (playwright@1.64.1)."""
    for k, w in enumerate(program):
        if Path(w).name.split("@", 1)[0] in PLAYWRIGHT:
            rest = program[k + 1:]
            if not rest:
                return None
            if rest[0] == "install-deps":
                return "deps"
            if rest[0] == "install":
                return "deps" if any(x == "--with-deps" or x.startswith("--with-deps=") for x in rest[1:]) else "browsers"
            return None
    return None


def cache_script_call(program: list[str]) -> list[str] | None:
    """apt-cache.sh's arguments, when the program is it."""
    if Path(program[0]).name == Path(CACHE_SCRIPT).name:
        return program[1:]
    return None


def normal_path(text: str) -> str:
    """A directory as a step's shell and a cache step's `path:` each spell it, in one form."""
    text = re.sub(r"\$\{\{\s*runner\.temp\s*\}\}", "$RUNNER_TEMP", text.strip())
    text = re.sub(r"\$\{\{\s*github\.workspace\s*\}\}", "$GITHUB_WORKSPACE", text)
    text = re.sub(r"\$\{(RUNNER_TEMP|GITHUB_WORKSPACE|HOME)\}", r"$\1", text)
    text = re.sub(r"^~(?=/|$)", "$HOME", text)
    return text.rstrip("/") or text


def hashed_files(key: str) -> set[str]:
    return {a or b for m in HASH_FILES.finditer(key) for a, b in QUOTED.findall(m.group(1))}


# --- The walk.
@dataclass
class Scan:
    problems: list[tuple[str, int, str]] = field(default_factory=list)
    workflows: int = 0
    actions: int = 0
    jobs: int = 0
    run_steps: int = 0
    cached: int = 0
    browsers: int = 0

    @property
    def summary(self) -> str:
        return (f"{self.workflows} required workflows and {self.actions} local actions, {self.jobs} jobs and "
                f"{self.run_steps} run steps read: {self.cached} package installs take the cached path and "
                f"{self.browsers} Playwright browser installs carry a timeout-minutes and no --with-deps")

    def refuse(self, path: str, line: int, *message: str) -> None:
        self.problems.append((path, line, "".join(message)))


def step_label(step, index: int) -> str:
    name = step.get("name") if isinstance(step, dict) else None
    return f'"{name}"' if name else f"{index + 1}"


def check_steps(path: str, where: str, steps, scan: Scan, actions: dict) -> None:
    """Every step of one job, or of one composite action. `where` says which ("job daemon-go")."""
    if not isinstance(steps, list):
        return
    restores: list[Mapping] = []
    for index, step in enumerate(steps):
        if not isinstance(step, Mapping):
            continue
        uses = str(step.get("uses") or "")
        if uses.startswith(("actions/cache/restore@", "actions/cache@")):
            restores.append(step)
        if uses.startswith("./"):
            actions.setdefault(uses.rstrip("/"), []).append(f"{path} {where}")
        run = step.get("run")
        if not isinstance(run, str):
            continue
        scan.run_steps += 1
        start = step.text_lines.get("run", step.lines.get("run", 1))
        label = f"{path}, {where}, step {step_label(step, index)}"
        for cmd in all_commands(run):
            k = program_index(cmd.words)
            if k is None:
                continue
            program = cmd.words[k:]
            line = start + cmd.line
            shown = " ".join(program)
            apt = apt_command(program)
            if apt is not None and apt not in APT_READS:
                scan.refuse(path, line, f"{label} runs `{shown}`, which reads an apt mirror while the job runs: ",
                            f"install the package from the job's apt cache with `{CACHE_SCRIPT} install LIST DIR ",
                            "PACKAGE` after a restore of DIR")
                continue
            if installs_a_file(program):
                scan.refuse(path, line, f"{label} runs `{shown}`, which installs a .deb this rule cannot trace to the ",
                            f"job's apt cache: install it with `{CACHE_SCRIPT} install` instead")
                continue
            playwright = playwright_install(program)
            if playwright == "deps":
                scan.refuse(path, line, f"{label} runs `{shown}`, whose host packages come from an apt mirror while ",
                            "the job runs: install the browser alone (no --with-deps), and any package it needs from ",
                            "the job's apt cache")
                continue
            if playwright == "browsers":
                if "timeout-minutes" in step:
                    scan.browsers += 1
                else:
                    scan.refuse(path, line, f"{label} runs `{shown}` with no timeout-minutes on its step: bound the ",
                                "download, as every Playwright browser install of a required job is bound")
                continue
            args = cache_script_call(program)
            if args is None:
                continue
            mode = args[0] if args else ""
            if mode == "image":
                continue
            if mode != "install":
                scan.refuse(path, line, f"{label} runs `{shown}`, which fetches from the network on every run: a ",
                            f"required job installs with `{CACHE_SCRIPT} install`, and ci-caches.yaml fills the cache")
                continue
            if len(args) < 4:
                scan.refuse(path, line, f"{label} runs `{shown}`, which names no LIST, DIR and PACKAGE")
                continue
            listed, directory = args[1], normal_path(args[2])
            missing = []
            if "timeout-minutes" not in step:
                missing.append("a timeout-minutes on its step, so a hung mirror on a miss ends at the step's bound")
            restored = [r for r in restores if directory in restored_paths(r)]
            if not restored:
                scope = "action" if where.startswith("the local action") else "job"
                missing.append(f"an actions/cache restore of {args[2]} before it in the same {scope}, so the install "
                               "finds the entry ci-caches.yaml filled")
            else:
                key = str((restored[-1].get("with") or {}).get("key") or "")
                names = hashed_files(key)
                if not key.startswith("apt-") or listed not in names or CACHE_SCRIPT not in names:
                    missing.append(f"a key on that restore that starts apt- and hashes {listed} and {CACHE_SCRIPT} "
                                   f"(it is `{key}`), so a change to either fills a new entry")
            if missing:
                scan.refuse(path, line, f"{label} runs `{shown}` without ", "; and without ".join(missing))
            else:
                scan.cached += 1


def restored_paths(step: Mapping) -> list[str]:
    """The directories an actions/cache step restores, each in normal_path's form."""
    return [normal_path(p) for p in str((step.get("with") or {}).get("path") or "").splitlines() if p.strip()]


def required_workflows(root: Path, scan: Scan) -> dict[str, Mapping]:
    """ci.yaml and every local workflow it calls, followed through each called file."""
    found: dict[str, Mapping] = {}
    todo = [ROOT_WORKFLOW]
    while todo:
        rel = todo.pop()
        if rel in found:
            continue
        target = root / rel
        if not target.is_file():
            if rel != ROOT_WORKFLOW:
                scan.refuse(rel, 1, f"{rel} is called by a required workflow and is not in the tree, so its steps ",
                            "are unread")
            continue
        try:
            document = load(target)
        except yaml.YAMLError as error:
            scan.refuse(rel, 1, f"{rel} is not valid YAML, so its steps are unread: ", " ".join(str(error).split()))
            continue
        if not isinstance(document, Mapping):
            continue
        found[rel] = document
        for job in (document.get("jobs") or {}).values():
            uses = str(job.get("uses") or "") if isinstance(job, dict) else ""
            if uses.startswith("./.github/workflows/"):
                todo.append(uses[2:])
    return found


def scan_tree(root: Path) -> Scan:
    scan = Scan()
    root = Path(root)
    workflows = required_workflows(root, scan)
    scan.workflows = len(workflows)
    actions: dict[str, list[str]] = {}
    for rel, document in sorted(workflows.items()):
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, Mapping) or "steps" not in job:
                continue
            scan.jobs += 1
            check_steps(rel, f"job {job_id}", job.get("steps"), scan, actions)
    done: set[str] = set()
    while set(actions) - done:
        uses = min(set(actions) - done)
        done.add(uses)
        directory = root / uses[2:]
        files = [directory / name for name in ("action.yml", "action.yaml") if (directory / name).is_file()]
        if not files:
            continue
        rel = str(files[0].relative_to(root))
        try:
            document = load(files[0])
        except yaml.YAMLError as error:
            scan.refuse(rel, 1, f"{rel} is not valid YAML, so its steps are unread: ", " ".join(str(error).split()))
            continue
        runs = document.get("runs") if isinstance(document, Mapping) else None
        if not isinstance(runs, Mapping) or runs.get("using") != "composite":
            continue
        scan.actions += 1
        callers = sorted(set(actions[uses]))
        shown = ", ".join(callers[:3]) + (f" and {len(callers) - 3} more" if len(callers) > 3 else "")
        check_steps(rel, f"the local action {uses[2:]} (used by {shown})", runs.get("steps"), scan, actions)
    return scan


def check(root) -> tuple[list[tuple[str, int, str]], str]:
    """(each refusal over the tree at root as (path, line, message), one line of what was read)."""
    scan = scan_tree(Path(root))
    if scan.problems:
        return scan.problems, (f"{len(scan.problems)} package installs a required job makes off the cached path, "
                               f"over the {scan.summary}")
    if scan.workflows == 0:
        return [], f"UNKNOWN: no {ROOT_WORKFLOW} under {root}, so no workflow is known to be required"
    if scan.run_steps == 0:
        return [], (f"UNKNOWN: read no run step of the {scan.workflows} required workflows under {root}, so their "
                    f"installs are unseen")
    return [], f"no required job installs a package off the cached path: {scan.summary}"


def main(argv: list[str]) -> int:
    root = Path(argv[1]) if len(argv) > 1 else Path(__file__).resolve().parents[2]
    problems, summary = check(root)
    if problems:
        print(f"{summary}:", file=sys.stderr)
        for path, line, message in problems:
            print(f"::error file={path},line={line}::{message}")
        return 1
    if summary.startswith("UNKNOWN: "):
        print(summary, file=sys.stderr)
        return 2
    print(summary)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
