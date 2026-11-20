#!/usr/bin/env python3
"""Run a `go test` command with each test binary in a PID and mount namespace of its own, whose new
/proc lists that binary's processes and none of the machine's, fail it unless every package that
has tests ran there, and list the tests that read a process table, each with the root it read.

The machine's /proc lists every process of every user on it, so a test whose code under test judges
by what it finds there passes or fails by what else the machine runs. packages/daemon refuses such
a listing at each of its scanners (packages/daemon/internal/hostproc); this script holds the routes
no scanner sees, a raw read of the directory, a joined glob, an opened file handed to a helper, a
program such as pgrep, by giving each test binary a /proc that holds none of the machine's
processes. CI runs every test binary of packages/daemon under it (docs/testing.md, "A test that
lists the machine's /proc").

Usage, from the module's directory:

    ../../.github/scripts/private_proc.py [--sudo] go test -count=1 ./internal/launcher/
    ../../.github/scripts/private_proc.py --check [--sudo]

It runs the command with `-exec` set to this script, so go runs each test binary through it. For
each binary it runs

    unshare --user --map-root-user --pid --fork --mount-proc --kill-child -- <python> private_proc.py --init user ...

an unprivileged user namespace whose root is the user, as a pod's launcher starts each role (`legion
launcher role-init`), so no sudo is needed where the kernel lets a user make its own namespaces: a
Legion pod, and a machine that sets no restriction. Where the kernel refuses that (GitHub's Ubuntu
runners do, under AppArmor, unless a job lifts the restriction), it runs instead, as root,

    sudo -n unshare --pid --fork --mount-proc --kill-child -- <python> private_proc.py --init sudo ...

and where neither works it refuses the command, naming both reasons, and runs no test. With --sudo it
takes the second route alone, and refuses, naming sudo's reason, where `sudo -n` does not answer: a
step whose tests run sudo themselves (a process of another user, a PID namespace made as root) passes
it, since sudo cannot make a process root inside a user namespace, where the uid that owns sudo is
not mapped. It says which route it took. Each route is tried with a probe that makes the namespace
and the binary's user in it, before any binary runs. --check prints the route the command would take,
or the refusal, and exits 0 or 1.

PID 1 of either namespace is this script's supervisor. It checks that it is PID 1, that the /proc it
sees is its namespace's own (its self link names 1) and that it lists no process but itself, then
starts the test binary beneath it as the user, groups, directory and environment go gave it, passes
it TERM, INT, HUP, QUIT, USR1 and USR2 when it is sent one (a PID 1 ignores a signal it has no
handler for), reaps every orphan of the namespace as a container's first process does, and exits
with the binary's status once the binary exits, which ends every process of the namespace with it.
In the user namespace the binary is the user's uid and gid again through a user namespace of its own
that maps them to the namespace's root, as role-init gives a role its uid; the user's other groups
are still its own, and read there as the overflow group, since the namespace maps none of them.
The test binary is no PID 1, so a launcher test that asks whether its process is the container's
first is answered as on any machine, and a test that wants a PID 1 of its own still makes one
inside (internal/launcher's runAsPIDOne).

The namespace holds what a test can list and read of other processes: /proc, and every route to it.
It does not hold a signal: the test binary shares its process group and session with go, this
script and the shell that started them, so a signal a test sends its own process group reaches them
too, as on any machine.

A namespace that cannot be made, or a supervisor that finds itself in no new one, fails that test
binary: nothing runs on the machine's /proc instead. Each supervisor that starts a binary writes the
binary's package directory to a record, and after the command this script holds the record to
`go list -test` over the same packages and build flags: every package with a test file must have
run in a namespace of its own.

Each test binary also gets a file of its own to record its scans in (HOSTPROC_RECORD, which
packages/daemon/internal/hostproc writes): a binary that links its module's internal/hostproc
writes one line as it starts, and one for each scan a scanner of the module makes, with the test
that made it, the scanner, the root and whether that root was the test's own, a mount of proc read
by a reason, or refused. The test is the top-level test up the chain of goroutines that started the
scanning one, and in a child a test starts of its own test binary, the test that started it, which
the child is told (hostproc.ChildEnv). After the command this script prints, package by package,
the tests that read a process table, each with the roots it read. The list is of the tests that ran
in this command: a test that does not run reads nothing.

What it could not read it says as UNKNOWN, and fails: a command whose packages cannot be listed or
have no test file, one in which no binary ran, a package with tests that ran in no namespace, a
binary that links internal/hostproc and recorded nothing, and a scan whose test the record could not
name. A go command that fails keeps its own exit status, after the list and any UNKNOWN.

Its one option is --sudo, before the go command; everything after that is the go command, which
must be `go test` and must not set -exec itself.
"""

import os
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

SCRIPT = Path(__file__).resolve()

# The go test flags whose value is the next word, as `go help testflag` and `go help build` list
# them: a word after one of these is its value and no package. read_go_args reads by this table,
# for this script and for rule 22 of check-ci-filters.sh (ci_workflow_rules.py), which imports it.
VALUE_FLAGS = frozenset({
    "-bench", "-benchtime", "-blockprofile", "-blockprofilerate", "-count", "-coverpkg", "-coverprofile",
    "-covermode", "-cpu", "-cpuprofile", "-exec", "-fuzz", "-fuzzcachedir", "-fuzzminimizetime", "-fuzztime",
    "-gcflags", "-ldflags", "-list", "-memprofile", "-memprofilerate", "-mutexprofile", "-mutexprofilefraction",
    "-o", "-outputdir", "-overlay", "-p", "-parallel", "-pkgdir", "-run", "-shuffle", "-skip", "-tags",
    "-timeout", "-trace", "-vet", "-asmflags", "-mod", "-modfile", "-pgo", "-toolexec", "-C",
})

# The flags that change which files a package's test build holds, and so which packages have a
# test file to run and what their binaries link: each is handed to `go list` as the command gave it.
BUILD_FLAGS = frozenset({"-tags", "-race"})

# What the supervisor exits with when it finds itself outside a namespace of its own.
NOT_PRIVATE = 125

# The variable naming the file a test binary records its scans in (hostproc.RecordEnv), the one
# that tells the binary go ran that it is that binary, and no child a test started of it
# (hostproc.BinaryEnv), and the package of the module whose binaries write the record.
RECORD_ENV = "HOSTPROC_RECORD"
BINARY_ENV = "HOSTPROC_BINARY"
RECORDER = "internal/hostproc"

# The two ways a test binary's namespace is made, in the order they are tried (USER first), with
# the command each runs the supervisor under and how it is named.
USER, SUDO = "user", "sudo"
MAKE = {
    USER: ["unshare", "--user", "--map-root-user", "--pid", "--fork", "--mount-proc", "--kill-child", "--"],
    SUDO: ["sudo", "-n", "unshare", "--pid", "--fork", "--mount-proc", "--kill-child", "--"],
}
NAMED = {USER: "an unprivileged user namespace (unshare --user --map-root-user)", SUDO: "sudo -n unshare"}
# The route run_command chose, for each run_binary of the command.
ROUTE_ENV = "PRIVATE_PROC_ROUTE"


def say(line: str) -> None:
    print(f"private-proc: {line}", file=sys.stderr, flush=True)


# What a bool flag's value may say for false, as Go's strconv.ParseBool reads it.
FALSE = frozenset({"0", "f", "F", "false", "FALSE", "False"})


def read_go_args(args: list[str], value_flags: frozenset = VALUE_FLAGS,
                 test: bool = True) -> tuple[list[tuple[str, str | None, list[str]]], list[str]]:
    """A go command's words after its subcommand, as cmd/go reads them: its flags, each as its name
    spelled with one leading `-`, its value (the text after `=`, the next word for a value_flags
    name given without `=`, or None) and the words that gave it, and its package arguments, every
    other word. For `go test`, `-args`, `--args` and `--` end the command's own words: every word
    from there is the test binary's (cmd/go's testFlags). For `go list` (test False) every word
    after `--` is a package. The one reader of a go command line for this script and for rule 22
    of check-ci-filters.sh, which each take what they judge by from its answer."""
    flags: list[tuple[str, str | None, list[str]]] = []
    packages: list[str] = []
    index = 0
    while index < len(args):
        word = args[index]
        if word == "--":
            if not test:
                packages += args[index + 1:]
            break
        if test and word in ("-args", "--args"):
            break
        if not word.startswith("-"):
            packages.append(word)
            index += 1
            continue
        spelled, equals, value = word.partition("=")
        name = spelled.replace("--", "-", 1)
        if equals:
            flags.append((name, value, [word]))
        elif name in value_flags and index + 1 < len(args):
            flags.append((name, args[index + 1], [word, args[index + 1]]))
        else:
            flags.append((name, None, [word]))
        index += len(flags[-1][2])
    return flags, packages


def split_go_test(args: list[str]) -> tuple[list[str], list[str]]:
    """The build flags `go list` must be given and the packages of a `go test` command's arguments
    (the words after `go test`, read_go_args's reading). A command naming no package tests the
    package in its directory."""
    flags, packages = read_go_args(args)
    return [word for name, _, words in flags if name in BUILD_FLAGS for word in words], packages or ["."]


def expected_dirs(build: list[str], packages: list[str]) -> dict[str, bool]:
    """Every package directory of the command that holds a test file, as `go list -test` names its
    test binary, and whether that binary links its module's internal/hostproc, which records the
    binary's scans."""
    listed = subprocess.run(
        ["go", "list", *build, "-test", "-f",
         "{{.ImportPath}}\t{{.Dir}}\t{{with .Module}}{{.Path}}{{end}}\t{{join .Deps \" \"}}", *packages],
        capture_output=True, text=True, check=False)
    if listed.returncode != 0:
        raise RuntimeError(f"go list exited {listed.returncode}: {listed.stderr.strip()}")
    expected = {}
    for line in listed.stdout.splitlines():
        path, directory, module, deps = (line.split("\t") + ["", "", ""])[:4]
        if not path.endswith(".test") or not directory:
            continue
        recorder = f"{module}/{RECORDER}"
        links = any(dep == recorder or dep.startswith(recorder + " [") for dep in deps.split())
        expected[os.path.realpath(directory)] = links
    return expected


@dataclass
class Binary:
    """What the test binaries of one package directory left in the command's record of scans."""
    codes: list[int] = field(default_factory=list)
    started: int = 0  # binary lines: processes of the binary that opened their scan file
    scans: set[tuple[str, str, str, str, str]] = field(default_factory=set)  # (test, scanner, kind, root, reason)


def gather(scans: Path) -> dict[str, Binary]:
    """Each package directory's record, from the scan file run_binary gave each binary of it: the
    package line run_binary wrote before the binary started, the lines the binary and its children
    wrote, and the exit line run_binary wrote after it, which a killed run_binary never writes."""
    binaries: dict[str, Binary] = {}
    for path in sorted(scans.glob("*.scans")):
        lines = [line.split("\t") for line in path.read_text().splitlines()]
        if not lines or lines[0][0] != "package" or len(lines[0]) != 2:
            continue
        binary = binaries.setdefault(os.path.realpath(lines[0][1]), Binary())
        for fields in lines[1:]:
            if fields[0] == "binary":
                binary.started += 1
            elif fields[0] == "scan" and len(fields) == 6:
                _, kind, test, scanner, root, reason = fields
                binary.scans.add((test, scanner, kind, root, reason))
            elif fields[0] == "exit" and len(fields) == 2:
                binary.codes.append(int(fields[1]))
    return binaries


def scan_said(scanner: str, kind: str, root: str, reason: str, codes: list[int]) -> str:
    """One scan of the list (hostproc.ScanOwn and the other kinds): its scanner, whether the root it
    read was the test's own or the host's, the root, and a host root's reason or its refusal, with
    what the package's binary exited with (a test that expects a refusal sees it in a child)."""
    if kind == "own":
        return f"{scanner}, own {root}"
    if kind == "namespace":
        return f"{scanner}, own {root} (the /proc of a PID namespace whose PID 1 is the test's process)"
    if kind == "reason":
        return f"{scanner}, host {root}, by its reason: {reason}"
    exited = " and ".join(str(code) for code in codes) or "with no status this script read"
    return f"refused: {scanner}, host {root}, with no reason (the package's binary exited {exited})"


def print_list(cwd: str, ran: set[str], expected: dict[str, bool], binaries: dict[str, Binary]) -> None:
    """The tests of each package that ran which read a process table, each with the roots it read.
    A scan whose test the record could not name is no test's, and is counted here and said under
    UNKNOWN (unnamed_scans); a binary that recorded nothing gets no line, only its UNKNOWN."""
    say("the tests that read a process table, by package; a test that did not run here read nothing, so this lists "
        "this command's tests alone (own: a root of the test's own; host: a mount of proc, the machine's /proc in a "
        "test run by hand and the binary's private one here):")
    for directory in sorted(ran):
        name = os.path.relpath(directory, cwd)
        binary = binaries.get(directory, Binary())
        if not expected.get(directory, False):
            say(f"  {name}: no scanner of the module is in its test binary (it does not link {RECORDER})")
            continue
        if not binary.started:
            continue  # an UNKNOWN of its own
        tests: dict[str, list[str]] = {}
        for test, scanner, kind, root, reason in sorted(binary.scans):
            if test:
                tests.setdefault(test, []).append(scan_said(scanner, kind, root, reason, binary.codes))
        unnamed = len(binary.scans) - sum(len(scans) for scans in tests.values())
        if not tests and not unnamed:
            say(f"  {name}: no test read a process table")
            continue
        if tests:
            say(f"  {name}: {len(tests)} tests read a process table")
        for test, scans in tests.items():
            say(f"    {test}: " + "; ".join(scans))
        if unnamed:
            say(f"  {name}: {unnamed} scans whose test is not known, said under UNKNOWN below")


def unnamed_scans(cwd: str, ran: set[str], expected: dict[str, bool], binaries: dict[str, Binary]) -> list[str]:
    """An UNKNOWN for each package with a scan its record names no test for: a scan in a child of the
    test binary that no test named (hostproc.ChildEnv), or one no goroutine of a running test
    started. A list that put it under a test would name a test that may not have made it."""
    unknown = []
    for directory in sorted(ran):
        binary = binaries.get(directory, Binary())
        scans = sorted(scan for scan in binary.scans if not scan[0])
        if expected.get(directory, False) and scans:
            unknown.append(f"{os.path.relpath(directory, cwd)} made {len(scans)} scans whose test its record could not "
                           "name (a scan in a child of the test binary that no test named with hostproc.ChildEnv, or one "
                           "that no goroutine of a running test started), so which test read them is not known: "
                           + "; ".join(scan_said(scanner, kind, root, reason, binary.codes)
                                       for _, scanner, kind, root, reason in scans))
    return unknown


def identity() -> tuple[str, str, str]:
    """This user's uid, gid and groups, as the supervisor starts a test binary with them."""
    return str(os.getuid()), str(os.getgid()), ",".join(str(group) for group in os.getgroups()) or str(os.getgid())


def refusal(route: str) -> str:
    """'' when route makes a namespace on this machine whose supervisor can start a binary as this
    user, which a probe in such a namespace shows; else why not, from what the probe printed."""
    if shutil.which(MAKE[route][0]) is None:
        return f"{NAMED[route]}: no {MAKE[route][0]} is on PATH"
    try:
        probe = subprocess.run([*MAKE[route], sys.executable, str(SCRIPT), "--probe", route, *identity()],
                               stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=60, check=False)
    except subprocess.TimeoutExpired:
        return f"{NAMED[route]}: its probe did not end within 60 s"
    if probe.returncode == 0:
        return ""
    said = [line.strip().removeprefix("private-proc: ") for line in (probe.stderr + probe.stdout).splitlines() if line.strip()]
    return f"{NAMED[route]}: " + (said[-1] if said else f"its probe exited {probe.returncode}")


def choose_route(sudo_only: bool) -> tuple[str, str]:
    """The route each test binary's namespace is made by and the line that says why, or "" and the
    refusal naming every route's reason: USER, else SUDO where the kernel refuses USER; SUDO alone
    with --sudo, which never falls back to USER."""
    if sudo_only:
        why = refusal(SUDO)
        if why:
            return "", f"--sudo takes {NAMED[SUDO]} alone, which cannot make one here: {why}"
        return SUDO, f"each test binary's namespace is made with {NAMED[SUDO]}, as --sudo asks"
    why_user = refusal(USER)
    if not why_user:
        return USER, f"each test binary's namespace is made with {NAMED[USER]}"
    why_sudo = refusal(SUDO)
    if not why_sudo:
        return SUDO, f"each test binary's namespace is made with {NAMED[SUDO]}, since this machine refuses the first way: {why_user}"
    return "", f"neither way can make one here: {why_user}; {why_sudo}"


def check(argv: list[str]) -> int:
    """--check [--sudo]: the route a command would take, or its refusal."""
    if argv not in ([], ["--sudo"]):
        say(f"--check takes --sudo or nothing; got {shlex.join(argv)}")
        return 2
    route, said = choose_route(argv == ["--sudo"])
    say(said if route else f"a test binary cannot have a PID namespace of its own: {said}")
    return 0 if route else 1


def run_command(argv: list[str]) -> int:
    """The wrapper: go test with -exec set to this script, then the record held to go list and the
    list of what the tests read."""
    sudo_only = argv[:1] == ["--sudo"]
    argv = argv[1:] if sudo_only else argv
    if argv[:2] != ["go", "test"]:
        say(f"give a go test command, for example: private_proc.py [--sudo] go test ./...; got {shlex.join(argv)}")
        return 2
    rest = argv[2:]
    if any(name == "-exec" for name, _, _ in read_go_args(rest)[0]):
        say("the command sets -exec itself, which would run its test binaries outside a namespace of their own")
        return 2
    route, said = choose_route(sudo_only)
    if not route:
        say(f"a test binary cannot have a PID namespace of its own, so no test ran, and none ran on the machine's /proc "
            f"instead: {said}")
        return 1
    say(said)
    build, packages = split_go_test(rest)
    try:
        expected = expected_dirs(build, packages)
    except RuntimeError as error:
        say(f"UNKNOWN: the command's packages cannot be listed, so nothing was read: {error}")
        return 1
    if not expected:
        say(f"UNKNOWN: no package of {shlex.join(packages)} has a test file, so no test ran and nothing was read")
        return 1
    fd, record = tempfile.mkstemp(prefix="private-proc-record-")
    os.close(fd)
    scans = Path(tempfile.mkdtemp(prefix="private-proc-scans-"))
    try:
        env = dict(os.environ, PRIVATE_PROC_RECORD=record, PRIVATE_PROC_SCANS=str(scans), **{ROUTE_ENV: route})
        exec_word = shlex.join([sys.executable, str(SCRIPT), "--run"])
        code = subprocess.run(["go", "test", "-exec", exec_word, *rest], env=env, check=False).returncode
        ran = {os.path.realpath(line) for line in Path(record).read_text().splitlines() if line.strip()}
        binaries = gather(scans)
    finally:
        os.unlink(record)
        shutil.rmtree(scans, ignore_errors=True)
    cwd = os.getcwd()
    unknown = []
    if not ran:
        unknown.append("no test binary ran in a namespace of its own"
                       + (", the go command having failed" if code else "") + ", so nothing was read")
    missing = sorted(set(expected) - ran)
    if ran and missing:
        unknown.append(f"{len(missing)} of the {len(expected)} packages with tests ran in no namespace of their own, so "
                       "what their tests read is not known: " + ", ".join(os.path.relpath(d, cwd) for d in missing))
    silent = sorted(d for d in ran if expected.get(d) and not binaries.get(d, Binary()).started)
    if silent:
        unknown.append(f"the test binary of {', '.join(os.path.relpath(d, cwd) for d in silent)} links {RECORDER} and "
                       "recorded nothing, so what its tests read is not known")
    unknown += unnamed_scans(cwd, ran, expected, binaries)
    if ran:
        print_list(cwd, ran, expected, binaries)
    for line in unknown:
        say(f"UNKNOWN: {line}")
    if code != 0:
        return code
    if unknown:
        return 1
    say(f"{len(ran)} test binaries ran, each beneath PID 1 of a PID namespace of its own, of the {len(expected)} "
        "packages with tests")
    return 0


def run_binary(argv: list[str]) -> int:
    """What go runs in place of a test binary: the binary, beneath this script's supervisor, in a
    PID and mount namespace of its own made by the command's route, with a file of its own to record
    its scans in. The environment goes to the supervisor in a file only this user and root can read,
    rather than on its command line, which every user of the machine can."""
    if not argv:
        say("--run names no test binary")
        return 2
    record = os.environ.get("PRIVATE_PROC_RECORD", "")
    scans = os.environ.get("PRIVATE_PROC_SCANS", "")
    route = os.environ.get(ROUTE_ENV, "")
    if not record or not scans or route not in MAKE:
        say(f"PRIVATE_PROC_RECORD, PRIVATE_PROC_SCANS or {ROUTE_ENV} is not set: run the go command through this script, "
            "not the binary alone")
        return 2
    fd, scanned = tempfile.mkstemp(dir=scans, prefix="binary-", suffix=".scans")
    with os.fdopen(fd, "w", encoding="utf-8") as file:
        file.write(f"package\t{os.getcwd()}\n")
    fd, environ = tempfile.mkstemp(prefix="private-proc-env-")
    with os.fdopen(fd, "wb") as file:
        file.write(b"\0".join(f"{name}={value}".encode()
                              for name, value in dict(os.environ, **{RECORD_ENV: scanned, BINARY_ENV: "1"}).items()))
    command = [*MAKE[route], sys.executable, str(SCRIPT), "--init", route, environ, *identity(), os.getcwd(), record,
               "--", *argv]
    maker = MAKE[route][0]
    # The namespace's maker (unshare, or sudo) runs as a child this process waits for, rather than in
    # its place, so the file of the environment is gone however the namespace ends, one that could
    # not be made included, and the signals this process takes are sent on to the maker.
    try:
        process = subprocess.Popen(command)
    except OSError as error:
        os.unlink(environ)
        say(f"cannot run {maker} to make the namespace: {error}")
        return 1
    for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP, signal.SIGQUIT):
        signal.signal(number, lambda received, _frame: process.send_signal(received))
    code = process.wait()
    code = code if code >= 0 else 128 - code
    with open(scanned, "a", encoding="utf-8") as file:
        file.write(f"exit\t{code}\n")
    if os.path.exists(environ):
        os.unlink(environ)
        say(f"the namespace for {argv[0]} was not made ({maker} exited {code}); nothing ran on the machine's /proc instead")
        return code or 1
    return code


def private_namespace() -> str:
    """'' when this process is PID 1 of a PID namespace whose /proc is its own and lists no other
    process; else what is wrong."""
    if os.getpid() != 1:
        return f"the supervisor is pid {os.getpid()}, not PID 1 of a namespace of its own"
    self_link = os.readlink("/proc/self")
    if self_link != "1":
        return f"/proc/self names {self_link}: this /proc is an outer namespace's, not this one's"
    others = sorted(int(name) for name in os.listdir("/proc") if name.isdigit() and name != "1")
    if others:
        return f"/proc lists {others} beside the supervisor: it is no new /proc"
    return ""


def become_user(route: str, uid: int, gid: int, groups: list[int]) -> None:
    """Make this process, the supervisor's child, the user go ran the binary as. Through sudo it is
    root and takes the user's groups, gid and uid. In the user namespace it is the user already,
    as the namespace's root: a user namespace of its own, whose one uid and gid map to that root,
    makes it the user's uid and gid again, with no capability once it execs, as role-init gives a
    role its uid. Its groups stay the user's, which that namespace cannot change."""
    if route == SUDO:
        os.setgroups(groups)
        os.setgid(gid)
        os.setuid(uid)
        return
    os.unshare(os.CLONE_NEWUSER)
    for name, text in (("setgroups", "deny"), ("gid_map", f"{gid} 0 1\n"), ("uid_map", f"{uid} 0 1\n")):
        with open(f"/proc/self/{name}", "w", encoding="ascii") as file:
            file.write(text)
    if (os.getuid(), os.getgid()) != (uid, gid):
        raise OSError(f"the user namespace made uid {os.getuid()} and gid {os.getgid()}, not {uid} and {gid}")


def probe(argv: list[str]) -> int:
    """--probe ROUTE UID GID GROUPS, run where ROUTE made a namespace: 0 when this process is PID 1
    of a namespace of its own and a child of it can become the user (become_user); else what failed,
    on stderr, for refusal to read."""
    route, uid, gid, groups = argv
    wrong = private_namespace()
    if wrong:
        say(wrong)
        return NOT_PRIVATE
    child = os.fork()
    if child == 0:
        try:
            become_user(route, int(uid), int(gid), [int(group) for group in groups.split(",")])
        except OSError as error:
            say(f"a binary cannot run there as uid {uid}: {error}")
            os._exit(1)
        os._exit(0)
    return os.waitstatus_to_exitcode(os.waitpid(child, 0)[1])


def supervise(argv: list[str]) -> int:
    """PID 1 of the namespace: check it, start the test binary as its user, forward signals, reap
    every orphan, and exit with the binary's status."""
    if len(argv) < 9 or argv[0] not in MAKE or argv[7] != "--":
        say("--init takes its route and six words, then --, then the test binary")
        return 2
    route, environ, uid, gid, groups, directory, record, _, *binary = argv
    try:
        data = Path(environ).read_bytes()
    finally:
        os.unlink(environ)
    wrong = private_namespace()
    if wrong:
        say(f"no namespace of its own for {binary[0]}: {wrong}")
        return NOT_PRIVATE
    env = dict(entry.split("=", 1) for entry in data.decode().split("\0") if "=" in entry)
    # The record is the wrapper's, made before go ran (run_command), and it is opened without
    # O_CREAT: through sudo this process is root, the file is the user's, and in a sticky directory
    # others can write, such as /tmp, where tempfile puts the record on a runner, Linux refuses even
    # root an O_CREAT open of another user's file when fs.protected_regular is set (Ubuntu sets it
    # to 2).
    fd = os.open(record, os.O_WRONLY | os.O_APPEND)
    with os.fdopen(fd, "w", encoding="utf-8") as file:
        file.write(directory + "\n")
    # The handlers come before the fork, so no signal sent the moment the binary starts is lost:
    # PID 1 of a namespace takes only the signals it has a handler for. An exec gives a caught
    # signal its default back, and the two Python ignores, SIGPIPE and SIGXFSZ, are given theirs
    # back in the child, so the binary and what it starts run as they would from a shell.
    child = 0

    def forward(received, _frame):
        if child:
            try:
                os.kill(child, received)
            except ProcessLookupError:
                pass

    for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP, signal.SIGQUIT, signal.SIGUSR1, signal.SIGUSR2):
        signal.signal(number, forward)
    child = os.fork()
    if child == 0:
        try:
            for number in (signal.SIGPIPE, signal.SIGXFSZ):
                signal.signal(number, signal.SIG_DFL)
            become_user(route, int(uid), int(gid), [int(group) for group in groups.split(",")])
            os.chdir(directory)
            os.execve(binary[0], binary, env)
        except OSError as error:
            say(f"cannot start {binary[0]}: {error}")
        os._exit(127)
    while True:
        try:
            pid, status = os.wait()
        except ChildProcessError:
            return 1
        if pid == child:
            code = os.waitstatus_to_exitcode(status)
            return code if code >= 0 else 128 - code


def main(argv: list[str]) -> int:
    if argv[:1] == ["--run"]:
        return run_binary(argv[1:])
    if argv[:1] == ["--init"]:
        return supervise(argv[1:])
    if argv[:1] == ["--probe"] and len(argv) == 5 and argv[1] in MAKE:
        return probe(argv[1:])
    if argv[:1] == ["--check"]:
        return check(argv[1:])
    return run_command(argv)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
