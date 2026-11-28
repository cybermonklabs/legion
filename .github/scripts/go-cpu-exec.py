#!/usr/bin/env python3
"""Run one Go binary for `go test -exec` or `go run -exec`, and record the CPU it took.

go runs `<this> <binary> <args...>` in the package's directory for each test binary it runs
(`go test -exec=<this>`, or GOFLAGS=-exec=<this>) and for the program of `go run -exec`. This
wrapper runs the binary unchanged: its arguments, its environment, its stdin, stdout and stderr,
its directory and the signals its caller left ignored are the binary's own, and SIGINT, SIGTERM,
SIGQUIT and SIGHUP sent to the wrapper are passed on to it. Then it writes one JSON row for the
binary into the record directory: the package's directory, the wall seconds, and the user and
system CPU seconds of the binary and of every descendant it waited for, with the binary's own and
its children's apart, so that a CI job can tell which package its CPU went to. Last it exits as
the binary did.

The record directory is $GO_CPU_DIR, else $RUNNER_TEMP/go-test-events/cpu. With neither set the
wrapper execs the binary in its own process and records nothing, so a run outside CI pays only
for this interpreter's start.

A row is `<dir>/<id>.json`, written whole under a dot name and renamed into place, so a reader
that lists the directory sees only whole rows. Its id is the time the binary started, in
nanoseconds, and the wrapper's pid, so two wrappers never share one. A test binary (a basename
ending `.test`) is given GO_CPU_PARENT=<id> in its environment, so a wrapper a test runs below it
(a test that runs `go test` or `go run` itself) names this row as its parent.

Once the binary has started the wrapper writes nothing to stdout or stderr, because under
`go test -json` both become the test's output events, and a row it cannot write is dropped in
silence, because no test result may depend on the record. A binary that cannot be started at all
is one line on stderr and exit 127, as a shell gives.

Standard library only. Linux only: it reads /proc and uses waitid and PR_SET_PDEATHSIG.
"""

import contextlib
import ctypes
import json
import os
import signal
import subprocess
import sys
import time

# The signals passed on to the binary. go sends a test binary SIGQUIT at its -timeout plus one
# minute and SIGKILL 5 s later, and the binary must get the SIGQUIT to print its goroutine dump;
# SIGINT, SIGTERM and SIGHUP are how a terminal or a runner stops a job.
FORWARDED = (signal.SIGINT, signal.SIGTERM, signal.SIGQUIT, signal.SIGHUP)

# Python ignores these at its start, and an exec keeps an ignored signal ignored, so the exec
# without a record puts them back to their default, where the binary would have them without this
# wrapper. subprocess does the same for the child it starts.
IGNORED_BY_PYTHON = (signal.SIGPIPE, signal.SIGXFSZ)

# prctl(2)'s option that has the kernel send the calling process a signal when its parent dies.
PR_SET_PDEATHSIG = 1

# The binary's pid once it has started; None before, and 0 once it has been reaped, after which
# its pid may be another process's. A signal that comes before it started waits in _early.
_child: int | None = None
_early: list[int] = []


def forward(number: int, frame: object) -> None:
    """The handler of each FORWARDED signal: pass it on to the binary."""
    if _child is None:
        _early.append(number)
    elif _child:
        # The binary may have ended in the meantime, and the wrapper must not fail for that.
        with contextlib.suppress(OSError):
            os.kill(_child, number)


def record_directory() -> str | None:
    """Where the row goes, or None when nothing is to be recorded."""
    directory = os.environ.get("GO_CPU_DIR")
    if directory:
        return directory
    runner_temp = os.environ.get("RUNNER_TEMP")
    if runner_temp:
        return os.path.join(runner_temp, "go-test-events", "cpu")
    return None


def exec_environment() -> dict[bytes, bytes]:
    """The environment this process was started with, as its exec gave it, in its order. It is
    not os.environ: when the locale is C, Python sets LC_CTYPE in its own environment at its start
    (PEP 538), and a binary started with os.environ would inherit that."""
    try:
        with open("/proc/self/environ", "rb") as f:
            block = f.read()
    except OSError:
        return dict(os.environb)
    environment = {}
    for entry in block.split(b"\0"):
        name, equals, value = entry.partition(b"=")
        # An entry with no name or no `=` is none that an exec can be given back.
        if name and equals:
            environment[name] = value
    return environment


def cannot_start(error: Exception) -> None:
    """The binary could not be started: say why in one line and exit 127, as a shell does."""
    sys.stderr.write(f"go-cpu-exec: {error}\n")
    sys.stderr.flush()
    os._exit(127)


def become(argv: list[str]) -> None:
    """Exec the binary in this process: the path taken when nothing is to be recorded."""
    environment = exec_environment()
    for number in IGNORED_BY_PYTHON:
        signal.signal(number, signal.SIG_DFL)
    try:
        os.execvpe(argv[0], argv, environment)
    except OSError as error:
        cannot_start(error)


def proc_times(pid: int) -> tuple[float | None, ...]:
    """utime, stime, cutime and cstime of /proc/<pid>/stat, in seconds: the CPU of the process
    itself and of the children it waited for. It is read while the process is a zombie, after it
    has ended and before it is reaped. Each is None when the file cannot be read."""
    try:
        with open(f"/proc/{pid}/stat", "rb") as f:
            stat = f.read()
        # The comm field is in parentheses and may hold any byte, `)` too, so the fields after it
        # are those after its last `)`. The first of them is field 3, the state, so utime, field
        # 14, is the twelfth, and cstime, field 17, the fifteenth.
        fields = stat[stat.rindex(b")") + 1 :].split()
        tick = os.sysconf("SC_CLK_TCK")
        return tuple(int(fields[i]) / tick for i in range(11, 15))
    except (OSError, ValueError, IndexError):
        return (None, None, None, None)


def seconds(value: float | None) -> float | None:
    return None if value is None else round(value, 6)


def cwd_field() -> str:
    """The directory the binary ran in: relative to GITHUB_WORKSPACE when it is inside it, so rows
    of different runners compare, else absolute. The kernel names the directory by its physical
    path, so GITHUB_WORKSPACE is compared by its physical path too."""
    here = os.getcwd()
    workspace = os.environ.get("GITHUB_WORKSPACE")
    if workspace:
        root = os.path.realpath(workspace)
        if here == root or here.startswith(root.rstrip(os.sep) + os.sep):
            return os.path.relpath(here, root)
    return here


def write_row(directory: str, row_id: str, row: dict) -> None:
    """Write the row whole into <directory>/<row_id>.json: into a dot file beside it first, then
    renamed over it, so a reader never sees part of one. Raises OSError when it cannot, leaving no
    dot file behind."""
    os.makedirs(directory, exist_ok=True)
    temporary = os.path.join(directory, f".{row_id}.json.tmp")
    try:
        with open(temporary, "w", encoding="utf-8") as f:
            f.write(json.dumps(row) + "\n")
        os.rename(temporary, os.path.join(directory, f"{row_id}.json"))
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(temporary)
        raise


def end_as(status: int) -> None:
    """Exit as the binary did: with its exit status, or by the signal that ended it, which the
    wrapper's caller then sees as it would have seen the binary's."""
    if os.WIFSIGNALED(status):
        number = os.WTERMSIG(status)
        # SIGKILL and SIGSTOP take no handler, so there is none to reset.
        with contextlib.suppress(OSError, ValueError):
            signal.signal(number, signal.SIG_DFL)
        signal.pthread_sigmask(signal.SIG_UNBLOCK, [number])
        os.kill(os.getpid(), number)
        # Only a signal whose default is not to end the process gets here.
        os._exit(128 + number)
    os._exit(os.WEXITSTATUS(status))


def run_and_record(argv: list[str], directory: str) -> None:
    """Run the binary as a child, wait for it, write its row and exit as it did."""
    global _child
    wrapper_pid = os.getpid()
    go_pid = os.getppid()
    parent = os.environ.get("GO_CPU_PARENT") or None
    environment = exec_environment()
    try:
        prctl = ctypes.CDLL(None, use_errno=True).prctl
    except (OSError, AttributeError):
        prctl = None

    def in_child() -> None:
        # This runs in the child between its fork and its exec. The kernel SIGKILLs the child
        # when the wrapper dies, so a wrapper go SIGKILLs at its timeout leaves no binary running;
        # a wrapper that died before the prctl took hold has already left the child to another
        # parent, and the child ends itself as the kernel would have.
        if prctl is not None:
            prctl(PR_SET_PDEATHSIG, signal.SIGKILL, 0, 0, 0)
        if os.getppid() != wrapper_pid:
            os.kill(os.getpid(), signal.SIGKILL)

    # A signal the caller left ignored stays ignored, in the wrapper and so in the binary, which
    # it would have been without the wrapper too.
    for number in FORWARDED:
        if signal.getsignal(number) != signal.SIG_IGN:
            signal.signal(number, forward)

    start_ns = time.time_ns()
    started = time.monotonic_ns()
    row_id = f"{start_ns}-{wrapper_pid}"
    if os.path.basename(argv[0]).endswith(".test"):
        environment[b"GO_CPU_PARENT"] = row_id.encode()
    try:
        # close_fds=False: the binary gets every descriptor the wrapper was given, as an exec
        # would give it. The wrapper's own descriptors are close-on-exec, as Python opens them.
        process = subprocess.Popen(argv, env=environment, close_fds=False, preexec_fn=in_child)
    except (OSError, subprocess.SubprocessError) as error:
        cannot_start(error)
    _child = process.pid
    for number in _early:
        forward(number, None)

    # WNOWAIT leaves the binary a zombie, whose /proc/<pid>/stat still holds its own CPU and its
    # children's apart; the reap after it gives the rusage of the two together. Python retries
    # both waits itself when a forwarded signal interrupts them (PEP 475).
    os.waitid(os.P_PID, _child, os.WEXITED | os.WNOWAIT)
    wall = (time.monotonic_ns() - started) / 1e9
    self_user, self_sys, children_user, children_sys = proc_times(_child)
    # Forwarding stops before the reap, after which the pid may be another process's.
    pid, _child = _child, 0
    _, status, usage = os.wait4(pid, 0)

    try:
        write_row(
            directory,
            row_id,
            {
                "v": 1,
                "id": row_id,
                "parent": parent,
                "go_pid": go_pid,
                "job": os.environ.get("GITHUB_JOB") or None,
                "step": os.environ.get("GITHUB_ACTION") or None,
                "cwd": cwd_field(),
                "binary": os.path.basename(argv[0]),
                "args": argv[1:],
                "start": round(start_ns / 1e9, 6),
                "wall_s": seconds(wall),
                "user_s": seconds(usage.ru_utime),
                "sys_s": seconds(usage.ru_stime),
                "self_user_s": seconds(self_user),
                "self_sys_s": seconds(self_sys),
                "children_user_s": seconds(children_user),
                "children_sys_s": seconds(children_sys),
                "maxrss_kib": usage.ru_maxrss,
                "exit": os.WEXITSTATUS(status) if os.WIFEXITED(status) else None,
                "signal": os.WTERMSIG(status) if os.WIFSIGNALED(status) else None,
            },
        )
    except Exception:
        # Dropped in silence: no test result may depend on the record.
        pass
    end_as(status)


def main() -> None:
    argv = sys.argv[1:]
    if not argv:
        sys.stderr.write("go-cpu-exec: usage: go-cpu-exec.py <binary> [<argument>...]\n")
        os._exit(2)
    directory = record_directory()
    if directory is None:
        become(argv)
    else:
        run_and_record(argv, directory)


if __name__ == "__main__":
    main()
