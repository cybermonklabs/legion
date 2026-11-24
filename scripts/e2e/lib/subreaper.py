#!/usr/bin/env python3
"""subreaper.py: run a stage proof as a child subreaper, so every process it leaves behind stays below it.

usage:
  subreaper.py CMD [ARG]...

Marks this process a child subreaper (prctl PR_SET_CHILD_SUBREAPER, which execve keeps), sets
LEGION_E2E_SUBREAPER to `PID START` (its pid and its start, field 22 of /proc/PID/stat, so a pid
another process takes later names no subreaper), and executes CMD in its own process, so the pid
and the start stay the same and CMD, a stage proof's bash (lib/subreaper.sh runs it so), is the
subreaper. A process the proof starts that ends and leaves a child behind, a daemon's double fork
or an ssh-agent's, then gives that child to the proof's bash, or to a subreaper below it, and never
to pid 1 or to a subreaper above it: every process of the run is below the proof's shell for its
whole life. So the run's walk (procs_naming, in lib/process-tree.sh) can tell another session's
process from one of the run's by its line of parents when it cannot read its environment
(run_descendant), and a process of this user that another session left under pid 1 during the run
does not fail the run's teardown. Bash reaps a child it adopted when it exits, as it reaps its own.

Python ignores SIGPIPE and SIGXFSZ at its start, and an exec keeps an ignored signal ignored, which
the bash it runs cannot undo: a proof under it would read `seq 1 100000 | head -1` as 1 and a
`Broken pipe` instead of 141, and a write past `ulimit -f` as an error instead of SIGXFSZ. So both
go back to SIG_DFL before the exec, as a proof started without this wrapper has them. (Python
installs a handler for SIGINT too, but an exec resets a handled signal to its default by itself.)

Where the mark cannot be set, the kernel refusing it, this Python having no ctypes or no prctl, or
no /proc to read its own start from, it says why on stderr, sets LEGION_E2E_SUBREAPER to `PID START
refused` (START empty where it could not be read), which lib/subreaper.sh reads as done and
procs_naming as no subreaper, and executes CMD all the same. A CMD that cannot be executed exits
127 (not found) or 126.
"""

import os
import signal
import sys

PR_SET_CHILD_SUBREAPER = 36


def start_of(pid: int) -> str:
    """The start of PID in clock ticks since boot, field 22 of /proc/PID/stat, as proc_start reads
    it, or "" where it cannot be read (no /proc)."""
    try:
        with open(f"/proc/{pid}/stat", "rb") as stat:
            fields = stat.read().rsplit(b") ", 1)[1].split()
        return fields[19].decode()
    except (OSError, IndexError, UnicodeDecodeError):
        return ""


def mark() -> str:
    """Sets PR_SET_CHILD_SUBREAPER on this process, and answers why not where it could not."""
    try:
        import ctypes

        libc = ctypes.CDLL(None, use_errno=True)
        prctl = libc.prctl
    except (ImportError, OSError, AttributeError) as error:
        return f"no prctl to call ({type(error).__name__}: {error})"
    if prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
        return f"prctl(PR_SET_CHILD_SUBREAPER) failed: {os.strerror(ctypes.get_errno())}"
    return ""


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit("usage: subreaper.py CMD [ARG]...")
    pid = os.getpid()
    start = start_of(pid)
    shell = f"{pid} {start}"
    refused = mark() if start != "" else f"no start to read for process {pid} in /proc/{pid}/stat"
    if refused == "":
        os.environ["LEGION_E2E_SUBREAPER"] = shell
    else:
        os.environ["LEGION_E2E_SUBREAPER"] = f"{shell} refused"
        print(f"subreaper.py: {refused}; running {sys.argv[1]} without it", file=sys.stderr)
    for name in ("SIGPIPE", "SIGXFSZ"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), signal.SIG_DFL)
    try:
        os.execvp(sys.argv[1], sys.argv[1:])
    except FileNotFoundError as error:
        print(f"subreaper.py: {sys.argv[1]}: {error.strerror}", file=sys.stderr)
        sys.exit(127)
    except OSError as error:
        print(f"subreaper.py: {sys.argv[1]}: {error.strerror}", file=sys.stderr)
        sys.exit(126)


if __name__ == "__main__":
    main()
