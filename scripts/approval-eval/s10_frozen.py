"""The claim's judging stage's frozen code: S10's own files, loaded as point 2 fixes them.

`--code` is the code-txt file of point 2 as `dispatch doc-read` prints it: its two header lines,
then the text whose sha256 point 2 fixes, split into its five files by their `=== FILE: <name> ===`
lines, each checked against the sha256 point 2 gives it (`frozen_files`). Its `shadow.py` is kept
under `--out` and run from the bytes the code-txt held, checked again against point 2's sha256 and
against the kept copy just before it runs, with `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` set to
values that reach nothing while it loads and removed after (`load_shadow`). Its `subprocess` and
`urllib` raise on any use, and while any of S10's code runs (`sealed`) every way out of the process
raises for every module, under any name it was imported by: the socket constructors and lookups,
`subprocess.Popen` and `os`'s process calls, restored once its code returns (the driver's own
commands, `kubectl` and `bun`, run outside). `JUDGE_PROMPT` and `build_bundle` are checked against
point 2's `prompt_sha256` and `bundle_code_sha256`, `JUDGE_SCHEMA` against the keywords the answer
check reads, and the skill files against point 2's `skill_sha256` (`skill_files`); they are put
where its `skill_bytes` reads them, as the code-txt's own header says (`place_skill`). The driver,
`s10_judge.py`, replaces the reads of point 3 and nothing else.
"""

import sys

sys.dont_write_bytecode = True

import _posixsubprocess  # noqa: E402
import _socket  # noqa: E402
import contextlib  # noqa: E402
import inspect  # noqa: E402
import os  # noqa: E402
import re  # noqa: E402
import socket  # noqa: E402
import subprocess  # noqa: E402
import traceback  # noqa: E402
import types  # noqa: E402
from dataclasses import dataclass  # noqa: E402

import s10_sandbox as SANDBOX  # noqa: E402
import s10_transcripts as CHECKS  # noqa: E402
from s10_store import CannotRun, sha256_of  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))


@dataclass(frozen=True)
class Pins:
    """What point 2 fixes of the frozen code: a test of the driver hands in its own."""

    code_sha256: str
    files: dict
    prompt_sha256: str
    bundle_code_sha256: str
    skill_sha256: dict
    asked: str
    model: str
    work: str


# Point 2: the code-txt's text after its two header lines and the start of each file's sha256;
# `JUDGE_PROMPT`'s and `build_bundle`'s; the skill's files at the commit S10 froze; the model asked
# and the model that must serve it; and the directory the live prompts name the bundles under.
FROZEN = Pins(
    code_sha256="8c1f0ea604030ba779285862e184d5e7d4178ead7c89be989430d8b1c38e4e2d",
    files={"shadow.py": "8da30263", "render.py": "c00a796b", "round.py": "e8a7a343",
           "kernel_round.py": "a6107153", "tick.sh": "6238bc5f"},
    prompt_sha256="84f0f498e7e7d2622b3cdb553296356670331b5b8ee3550eafea2c9ac3877659",
    bundle_code_sha256="ef222a12033d1a3bd3a074a22d6896e0239c867c8acc0de8d977f32e29d5c004",
    skill_sha256=dict([
        ("SKILL.md", "67e522cb41bf13952446f313cd4b34b08b936d6a9938526bb0499e6c65755de9"),
        ("references/never-decide.md", "698e5af8120cc626cd66207cb74f5943146214ffab088eb9d65ec2d0b7f1d40a"),
    ]),
    asked="anthropic/claude-opus-5-5:xhigh",
    model="anthropic/claude-opus-5-5",
    work="/work/shadow",
)

# What `shadow.py` reads from the environment when it is loaded, given values that reach nothing.
UNREACHABLE = {"DISPATCH_URL": "s10-judge-no-dispatch:", "DISPATCH_TOKEN_FILE": "/nonexistent/s10-judge/token"}


class InputsGap(Exception):
    """S10 read something the inputs do not give as of the judged moment (point 3)."""


def frozen_files(path, pins):
    """The code-txt's five files, by name, each checked against point 2."""
    with open(path, "rb") as handle:
        raw = handle.read()
    parts = raw.split(b"\n", 2)
    if len(parts) < 3 or not parts[0].startswith(b"File "):
        raise CannotRun(f"{path} is not a code-txt file as `dispatch doc-read` prints one")
    text = parts[2]
    if sha256_of(text) != pins.code_sha256:
        raise CannotRun(f"{path} holds text whose sha256 is {sha256_of(text)}, and point 2 fixes {pins.code_sha256}")
    pieces = re.split(rb"^=== FILE: (\S+) ===\n", text, flags=re.M)
    names = [name.decode() for name in pieces[1::2]]
    if names != list(pins.files):
        raise CannotRun(f"{path} holds the files {names}, and point 2 names {list(pins.files)}")
    files = dict(zip(names, pieces[2::2]))
    for name, prefix in pins.files.items():
        if not sha256_of(files[name]).startswith(prefix):
            raise CannotRun(f"{name} in {path} has sha256 {sha256_of(files[name])}, and point 2 gives {prefix}…")
    return files


class _Refused:
    """A module S10 imported whose every use raises: a read the claim does not replace."""

    def __init__(self, name):
        self._name = name

    def __getattr__(self, attribute):
        raise InputsGap(f"S10 reached {self._name}.{attribute}, which no input of the claim gives")


def _refused(name):
    def refused(*arguments, **options):
        raise InputsGap(f"S10 reached {name}, which no input of the claim gives")
    return refused


# The `os` calls that run a command or start a process.
OS_COMMANDS = re.compile(r"^(system|popen|fork|forkpty|exec\w*|spawn\w*|posix_spawn\w*|kill|killpg)$")


@contextlib.contextmanager
def sealed():
    """While S10's code runs, every way out of the process raises, for every module and under any
    name it was imported by: a connection (`socket`'s constructors and lookups, and `_socket`'s), a
    command (`subprocess.Popen`, under every function of `subprocess`, and `_posixsubprocess`), and
    `os`'s process calls. All is restored once its code returns, so the driver's own `kubectl` and
    `bun` run as before."""
    targets = [(socket, ("socket", "create_connection", "socketpair", "fromfd", "getaddrinfo")),
               (_socket, ("socket",)), (subprocess, ("Popen",)), (_posixsubprocess, ("fork_exec",)),
               (os, tuple(name for name in dir(os) if OS_COMMANDS.match(name)))]
    saved = []
    try:
        for module, names in targets:
            for name in names:
                if hasattr(module, name):
                    saved.append((module, name, getattr(module, name)))
                    setattr(module, name, _refused(f"{module.__name__}.{name}"))
        yield
    finally:
        for module, name, value in reversed(saved):
            setattr(module, name, value)


def load_shadow(path, pins, data):
    """`shadow.py` loaded as written: `data`, the bytes the code-txt held, held once more to the
    sha256 point 2 gives it and to the kept copy at `path` its tracebacks and `inspect` read, then
    run from those bytes inside `sealed`, with what reaches out refused (the module's docstring
    names each), and its pins checked."""
    if not sha256_of(data).startswith(pins.files["shadow.py"]):
        raise CannotRun(f"shadow.py has sha256 {sha256_of(data)}, and point 2 gives {pins.files['shadow.py']}…")
    with open(path, "rb") as handle:
        if handle.read() != data:
            raise CannotRun(f"{path} is not the shadow.py of the code-txt it was kept from")
    shadow = types.ModuleType("s10_frozen_shadow")
    shadow.__file__ = path
    os.environ.update(UNREACHABLE)
    try:
        with sealed():
            exec(compile(data, path, "exec"), shadow.__dict__)  # noqa: S102 - the pinned frozen code
    finally:
        for name in UNREACHABLE:
            os.environ.pop(name, None)
    shadow.subprocess = _Refused("subprocess")
    shadow.urllib = _Refused("urllib")
    measured = {
        "JUDGE_PROMPT": (sha256_of(shadow.JUDGE_PROMPT), pins.prompt_sha256),
        "build_bundle": (sha256_of(inspect.getsource(shadow.build_bundle)), pins.bundle_code_sha256),
        "SKILL_SHA": (shadow.SKILL_SHA, pins.skill_sha256),
        "ASKED": (shadow.ASKED, pins.asked),
        "MODEL": (shadow.MODEL, pins.model),
        "WORK": (shadow.WORK, pins.work),
        "HARNESS_SKILL_DIR": (shadow.HARNESS_SKILL_DIR, SANDBOX.SKILL_DIR),
    }
    for name, (found, wanted) in measured.items():
        if found != wanted:
            raise CannotRun(f"the frozen code's {name} is {found!r}, and point 2 fixes {wanted!r}")
    refusal = CHECKS.schema_refusal(shadow.JUDGE_SCHEMA)
    if refusal is not None:
        raise CannotRun(refusal)
    return shadow


def skill_files(skill_path, never_decide_path, pins):
    """The skill's two files, by their path in the skill, each checked against point 2."""
    files = {}
    for name, path in (("SKILL.md", skill_path), ("references/never-decide.md", never_decide_path)):
        with open(path, "rb") as handle:
            files[name] = handle.read()
        if sha256_of(files[name]) != pins.skill_sha256[name]:
            raise CannotRun(f"{path} has sha256 {sha256_of(files[name])}, and point 2 fixes "
                            f"{pins.skill_sha256[name]} for {name}")
    return files


def place_skill(shadow, files):
    """The skill's files where `skill_bytes` reads them, which it checks again."""
    for name, data in files.items():
        target = os.path.join(shadow.SKILL_DIR, name)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "wb") as handle:
            handle.write(data)
    with sealed():
        shadow.skill_bytes()


def raised_by_frozen_code(error, shadow):
    """Whether S10's own code raised `error`: its traceback passes through the frozen file, and no
    frame of the driver's own files (this directory's) comes after the last one there."""
    frames = traceback.extract_tb(error.__traceback__)
    frozen = [index for index, frame in enumerate(frames) if frame.filename == shadow.__file__]
    if not frozen:
        return False
    return not any(os.path.dirname(os.path.abspath(frame.filename)) == HERE for frame in frames[frozen[-1] + 1:])
