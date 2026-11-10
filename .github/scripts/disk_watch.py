#!/usr/bin/env python3
"""Measure what a CI job ran on - the free disk of the path it writes to, the memory the machine
had left, and its one-minute load - second by second, and print the worst each of them reached.

Two kinds of job drive it. envoy-smoke (dispatch://INFRA-1085), whose live tests build container
images on the runner's disk, asks how close a step came to filling it: nothing else in that job's
log says how much room the step ever had, so a run that fails for space says only that it ran
out, never how much it was short by. The tool-fallback jobs (dispatch://INFRA-2605) ask what the
machine was doing while each of their steps ran: one of them lost its hosted runner in the middle
of a run and recorded nothing at all about the machine it had been running on.

Two steps of the job run it, around the steps being measured:

    .github/scripts/disk_watch.py start --path /tmp --state "$RUNNER_TEMP/disk-watch.json"
    ... the steps being measured ...
    .github/scripts/disk_watch.py report --state "$RUNNER_TEMP/disk-watch.json"

`start` prints what each file system the job writes to holds and has free - the watched path's,
the Go caches', Docker's data root's - writes the watched file system's free kB into the state
file as the run's "before" number, and leaves a detached sampler behind it. Every second the
sampler reads the three numbers, keeps the worst each has reached with the time it was read, and
appends that second to a series file, the state file's path with `.series` after it:

    machine: 2026-11-10T11:03:07Z free 14217216 kB, available memory 10385420 kB, load 1.42

A reading the kernel does not give is written `unknown` in place of its number, so a tick is a
line whatever the machine answers. `report` stops the sampler and prints the series whole, then
the worst of each reading with the time it was read, then the line the script began as:

    disk: before <N> kB, least <M> kB, used <N-M> kB, margin <ok|short>

where `ok` means the step ended with at least as much free disk as it used, that is: it started
with at least twice the disk it went on to need.

Each line of the series carries the second it was read in, and the job record times every step
boundary, so a reader lines the two up: what the machine had when a step began, and what it had
while that step ran. It does not show a runner being lost: the series reaches a reader only
through the report step's output, and a run whose runner stopped answering stores no log at all.
It is the headroom of every run that does store one, and the reading for a command that hangs
while the runner is alive.

The state crosses the two steps in a file rather than in $GITHUB_ENV because a step of this job
that writes $GITHUB_ENV or $GITHUB_PATH makes ci_filter_coverage.py's job_reason non-empty for
the whole job, which unreduces every go command of envoy-smoke and breaks the envoy_smoke gate
in .github/scripts/ci_job_reads.py. The job's docker-plugin step crosses steps through
$GITHUB_OUTPUT and a step env: for the same reason.

The report prints and never fails the job: it exits 0 for `margin short` as it does for
`margin ok`, and a missing or unreadable state file prints a line saying so and exits 0 too.
This change is the measurement, nothing else, so no run may go red because of it.

`sample` is the detached sampler's own mode, started by `start` and ended by `report`. It is not
for a workflow to call.
"""

import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time

# How often the sampler re-reads the machine. The steps it watches run for minutes, so a second
# is often enough to catch the trough and fine enough to put a reading either side of every step
# boundary. It costs one line of the series; the state file is written only when a reading is
# worse than one already held, which most seconds are not.
SAMPLE_SECONDS = 1.0

# The sampler gives up after this long on its own, so a job that is cancelled between the two
# steps - which leaves no `report` to stop it - leaves nothing running behind it either. The
# longest job that drives it is tool-fallback, whose own timeout is 75 minutes, so this leaves
# the sampler a quarter of an hour of room over the job it must outlive.
SAMPLER_MAX_SECONDS = 5400.0

# How long `report` waits for the sampler to go after SIGTERM before printing anyway. The sampler
# holds no lock and writes atomically, so a straggler can only lower a number that is already
# written.
STOP_SECONDS = 2.0

# How long `go env` or `docker info` may take before start gives up on it and measures what it
# can. Neither is the measurement, only a way of naming one more directory.
TOOL_SECONDS = 20.0


def total_free_kb(directory: str) -> tuple[int, int]:
    """The total and the free kB of the file system holding `directory`, read from the kernel
    (shutil.disk_usage, which is os.statvfs here), not parsed out of df. Free is what an
    unprivileged process may still write: df's "Available", which is what a CI step actually
    has, and which is the number disk_usage reports as free."""
    usage = shutil.disk_usage(directory)
    return usage.total // 1024, usage.free // 1024


def free_kb(directory: str) -> int:
    return total_free_kb(directory)[1]


def now_utc() -> str:
    """The time a reading was taken, RFC 3339 in UTC to the second: the resolution the job record
    gives a step boundary, which is what a reader lines these readings up against."""
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def available_memory_kb() -> int | None:
    """MemAvailable of /proc/meminfo in kB: what a new process may still be given without the
    machine swapping, which is nearer what a step has left than MemFree is. None where there is
    no /proc/meminfo to read, or no such line in it: a reading the kernel will not give is a gap
    in one tick, never a reason to stop sampling."""
    try:
        with open("/proc/meminfo", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1])
    except (OSError, IndexError, ValueError):
        return None
    return None


def one_minute_load() -> str | None:
    """The one-minute load of /proc/loadavg, kept as the text the kernel wrote rather than as a
    number of this script's own rounding, so the line carries the reading itself. None where
    there is nothing to read, or where what is there is not a number: a line of the series says
    `unknown` sooner than it says something no kernel wrote."""
    try:
        with open("/proc/loadavg", encoding="utf-8") as handle:
            said = handle.read().split()
    except OSError:
        return None
    if not said:
        return None
    try:
        float(said[0])
    except ValueError:
        return None
    return said[0]


def mount_point(directory: str) -> str:
    """Where the file system holding `directory` is mounted: the highest directory above it that
    is still on the same device."""
    here = os.path.realpath(directory)
    while not os.path.ismount(here):
        here = os.path.dirname(here)
    return here


def tool_says(argv: list[str]) -> str | None:
    """One line of output from a tool that may not be there: None when the program is not on
    PATH, when it fails, when it takes too long or when it says nothing, with a line saying
    which. A runner without Go, or with no Docker daemon answering, is not a failure here: the
    directories that tool would have named go unmeasured and the rest are measured."""
    spelled = " ".join(argv)
    if shutil.which(argv[0]) is None:
        print(f"disk: no {argv[0]} on PATH, so `{spelled}` names no directory")
        return None
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=TOOL_SECONDS)
    except (OSError, subprocess.SubprocessError) as problem:
        print(f"disk: `{spelled}` did not answer ({problem}), so it names no directory")
        return None
    answer = done.stdout.strip()
    if done.returncode != 0 or not answer:
        print(f"disk: `{spelled}` said nothing (status {done.returncode}), so it names no directory")
        return None
    return answer


def tool_directories() -> list[str]:
    """The directories the job's other big writers use, as their own tools name them: the Go
    build and module caches, and Docker's data root, where the images the live tests build go."""
    found = []
    for name in ("GOCACHE", "GOMODCACHE"):
        said = tool_says(["go", "env", name])
        if said:
            found.append(said)
    said = tool_says(["docker", "info", "--format", "{{.DockerRootDir}}"])
    if said:
        found.append(said)
    return found


def print_space(directories: list[str]) -> None:
    """What df -k would print for the file systems holding `directories`, one line per distinct
    file system, naming the directories on it. A directory that cannot be reached - a cache its
    tool names before anything has written it, say - is named as such and skipped."""
    systems: dict[int, dict] = {}
    for directory in directories:
        try:
            device = os.stat(directory).st_dev
            total, free = total_free_kb(directory)
            mount = mount_point(directory)
        except OSError as problem:
            print(f"disk: {directory} cannot be read ({problem.strerror}), skipped")
            continue
        found = systems.get(device)
        if found is None:
            systems[device] = {"dirs": [directory], "mount": mount, "total": total, "free": free}
        else:
            found["dirs"].append(directory)
    for system in systems.values():
        print(
            f"disk: {', '.join(system['dirs'])} on {system['mount']}: "
            f"total {system['total']} kB, free {system['free']} kB"
        )


def read_state(state_path: str) -> dict | None:
    """The state the sampler and the report share, or None when it is not there or not JSON."""
    try:
        with open(state_path, encoding="utf-8") as handle:
            state = json.load(handle)
    except (OSError, ValueError):
        return None
    return state if isinstance(state, dict) else None


def write_state(state_path: str, state: dict) -> None:
    """Write the state whole: into a temporary file beside it, then rename over it, so a reader
    between two samples sees the state before or the state after, never half of either."""
    temporary = f"{state_path}.{os.getpid()}.tmp"
    with open(temporary, "w", encoding="utf-8") as handle:
        json.dump(state, handle)
        handle.write("\n")
    os.replace(temporary, state_path)


def start(path: str, state_path: str) -> int:
    """Print what the job's file systems hold, write the "before" number, and leave a sampler
    watching `path`."""
    try:
        before = free_kb(path)
    except OSError as problem:
        print(f"disk: {path} cannot be read ({problem.strerror}), nothing to watch")
        return 0
    print_space([path] + tool_directories())
    # The sampler is started before the state is written: it sleeps a second before its first
    # read and skips a state file that is not there yet, so one write, with its pid in it, is
    # all the file ever needs.
    sampler = subprocess.Popen(
        [sys.executable, os.path.abspath(__file__), "sample", "--path", path, "--state", state_path],
        # The sampler outlives this step, so it must hold none of the step's pipes: a child still
        # holding stdout keeps the runner waiting for an end of file that never comes.
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    # The state carries this step's own reading of all three, so a sampler that never writes
    # still leaves a report with three numbers and a time beside each. The series line is the
    # sampler's: its first read is immediate, in this same second, so writing one here would
    # put two lines on one second.
    when = now_utc()
    memory = available_memory_kb()
    load = one_minute_load()
    state = {"path": path, "before_kb": before, "sampler_pid": sampler.pid}
    # Every reading this step made is the worst there has been so far, and it was read now, so
    # the report names a time beside each number from the start.
    keep_worst(state, "least_kb", before, when, greater=False)
    if memory is not None:
        keep_worst(state, "least_memory_kb", memory, when, greater=False)
    if load is not None:
        keep_worst(state, "greatest_load", load, when, greater=True)
    write_state(state_path, state)
    print(f"disk: watching {path}, free {before} kB, sampler pid {sampler.pid}")
    return 0


def series_path(state_path: str) -> str:
    """Where the tick-by-tick readings go: beside the state file, under the state file's own
    name, so the two steps of the job name one path between them and the report finds the other
    from it."""
    return f"{state_path}.series"


def written(reading: int | str | None) -> str:
    """A reading as the series spells it: `unknown` for one the kernel would not give, so a tick
    whose machine answered nothing is still a tick with a time on it."""
    return "unknown" if reading is None else str(reading)


def series_line(when: str, free: int | None, memory: int | None, load: str | None) -> str:
    """One second of the machine, as the sampler writes it into the series."""
    return (
        f"machine: {when} free {written(free)} kB, "
        f"available memory {written(memory)} kB, load {written(load)}"
    )


def append_series(state_path: str, line: str) -> None:
    """Add one tick to the series. The file is opened, appended to and closed within the tick, so
    a sampler the report stops partway leaves whole lines behind it and no half of one."""
    try:
        with open(series_path(state_path), "a", encoding="utf-8") as handle:
            handle.write(f"{line}\n")
    except OSError:
        # A series that cannot be written is no reason to stop reading the machine: the file
        # system refusing the line is itself one of the things the next tick measures.
        return


def keep_worst(state: dict, name: str, value: int | str, when: str, *, greater: bool) -> bool:
    """Put `value` into the state under `name`, with the time it was read beside it as
    `<name>_at`, when it is worse than what is held there: the greatest for the load, the least
    for the two readings that run out. True when the state changed, which is what decides whether
    the state is written at all."""
    try:
        held = float(state[name])
        worse = float(value) > held if greater else float(value) < held
    except (KeyError, TypeError, ValueError):
        worse = True  # nothing readable is held, so this reading is the worst there has been
    if not worse:
        return False
    state[name] = value
    state[f"{name}_at"] = when
    return True


def sample(path: str, state_path: str) -> int:
    """The detached sampler: write the machine to the series once a second, and keep the worst
    each reading has reached in the state, until the report stops this process or the deadline
    above passes."""
    deadline = time.monotonic() + SAMPLE_SECONDS + SAMPLER_MAX_SECONDS
    while time.monotonic() < deadline:
        when = now_utc()
        try:
            free = free_kb(path)
        except OSError:
            free = None
        memory = available_memory_kb()
        load = one_minute_load()
        # The tick is written whatever the kernel gave, and before the state is read: a reader
        # counts the seconds the machine answered out of this file, so a second left out of it
        # would read as a second the machine was gone.
        append_series(state_path, series_line(when, free, memory, load))
        state = read_state(state_path)
        if state is not None:
            changed = False
            if free is not None:
                changed |= keep_worst(state, "least_kb", free, when, greater=False)
            if memory is not None:
                changed |= keep_worst(state, "least_memory_kb", memory, when, greater=False)
            if load is not None:
                changed |= keep_worst(state, "greatest_load", load, when, greater=True)
            # Nearly every tick finds the state already saying all three, and the state file is
            # on the very file system being measured, so a tick that would rewrite the same
            # bytes writes nothing.
            if changed:
                write_state(state_path, state)
        # The read comes before the sleep, so the series begins in the second `start` spawned
        # this process in rather than a second later: the first step being measured begins at
        # the boundary `start` ends on, and a reader lines that boundary up against a tick. The
        # state file may not be written yet on this first pass, which costs nothing: `start`
        # seeds all three worst readings itself, and the series line is written either way.
        time.sleep(SAMPLE_SECONDS)
    return 0


def stop_sampler(pid) -> None:
    """Stop the sampler named in the state file. A pid that is already gone, or a state file
    naming none, is no failure: the report measures, it does not police."""
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0:
        return
    try:
        os.kill(pid, signal.SIGTERM)
    except OSError:
        return
    deadline = time.monotonic() + STOP_SECONDS
    while time.monotonic() < deadline:
        try:
            os.kill(pid, 0)
        except OSError:
            return
        time.sleep(0.05)


def print_series(state_path: str) -> None:
    """Print the series the sampler wrote, tick by tick and word for word. It goes into the log
    whole because the job record times the steps and nothing else in it says what the machine
    held in the second a step ran."""
    path = series_path(state_path)
    try:
        with open(path, encoding="utf-8") as handle:
            ticks = handle.read().splitlines()
    except OSError:
        print(f"machine: no series file to read at {path}, no ticks were written")
        return
    for tick in ticks:
        print(tick)


def print_worst(state: dict) -> None:
    """The worst each reading reached and the time it was read. A reading the state holds no
    number or no time for is one no tick ever made, and is named as such rather than printed as
    a number with nothing behind it."""
    for name, direction, reading, unit in (
        ("least_kb", "least", "free disk", " kB"),
        ("least_memory_kb", "least", "available memory", " kB"),
        ("greatest_load", "greatest", "one-minute load", ""),
    ):
        value = state.get(name)
        when = state.get(f"{name}_at")
        if value is None or when is None:
            print(f"machine: no {reading} was read")
            continue
        print(f"machine: {direction} {reading} {value}{unit} at {when}")


def report(state_path: str) -> int:
    """Stop the sampler and print what the job ran on: every tick of the series, the worst each
    reading reached, and the one line this script began as. Always exits 0."""
    state = read_state(state_path)
    if state is None:
        print(f"disk: no state file to read at {state_path}, nothing measured")
        return 0
    stop_sampler(state.get("sampler_pid"))
    final = read_state(state_path) or state
    # The series first, then the worst of it, then the margin: the log reads from the whole run
    # down to the one number a person came for.
    print_series(state_path)
    print_worst(final)
    try:
        before = int(final["before_kb"])
        least = int(final["least_kb"])
    except (KeyError, TypeError, ValueError):
        print(f"disk: the state at {state_path} carries no numbers, nothing measured")
        return 0
    used = before - least
    margin = "ok" if least >= used else "short"
    print(f"disk: before {before} kB, least {least} kB, used {used} kB, margin {margin}")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="disk_watch.py",
        description="Measure the disk, the memory and the load a CI job ran on.",
    )
    # `sample` is in the parser but out of the help: `start` starts it, and nothing else should.
    modes = parser.add_subparsers(dest="mode", required=True, metavar="{start,report}")
    starting = modes.add_parser("start", help="record the free space and start the sampler")
    starting.add_argument("--path", required=True, help="the directory whose file system is watched")
    starting.add_argument("--state", required=True, help="the file the two steps share")
    reporting = modes.add_parser("report", help="stop the sampler and print what the job ran on")
    reporting.add_argument("--state", required=True, help="the file `start` wrote")
    sampling = modes.add_parser("sample")
    sampling.add_argument("--path", required=True)
    sampling.add_argument("--state", required=True)

    args = parser.parse_args(argv)
    if args.mode == "start":
        return start(args.path, args.state)
    if args.mode == "sample":
        return sample(args.path, args.state)
    return report(args.state)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
