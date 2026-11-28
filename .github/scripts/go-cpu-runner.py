#!/usr/bin/env python3
"""Record the runner a CI job ran on and the CPU the whole job used, beside the rows go-cpu-exec.py
writes for each Go binary the job runs.

A row says what one test binary took. It does not say what machine the binary ran on, and runners
of one label differ in their CPU, so two jobs' rows compare only once each job says which CPU it
had and how fast it hashed; nor does it say what the job used outside its binaries: the compile,
the Postgres service, the runner's own work. Two steps of the job run this script, around the
steps being measured:

    .github/scripts/go-cpu-runner.py start [--no-sampler]
    ... the job's go test steps ...
    .github/scripts/go-cpu-runner.py report [--no-rows] [--postgres NAME=CONTAINER]...

Every file goes under D = $RUNNER_TEMP/go-test-events/cpu, where go-cpu-exec.py writes the rows,
and the runner's own files under D/runner/, so the job's one upload of $RUNNER_TEMP/go-test-events/
carries both.

`start` prints one line:

    go-cpu runner: part=<P> cpus=<n> mem_kib=<n> kernel=<release> probe_one=<MB/s> probe_all=<MB/s> cache_entries=<n> goflags="<GOFLAGS>" model="<model>"

part is each distinct `CPU part` of /proc/cpuinfo with the number of CPUs that have it,
`value:count` joined by `,` in the order of first appearance (`0xd49:4`), and empty on a machine
whose cpuinfo names none (x86_64). cpus is the number of CPUs this process may run on, which
nproc prints. mem_kib is MemTotal of /proc/meminfo. kernel is the release uname -r prints.
probe_one and probe_all are a fixed SHA-256 probe of the CPU: the time to hash one fixed 256 MiB
buffer once on one thread, and then on cpus threads at once, each hashing the whole buffer
(hashlib lets go of the interpreter's lock while it hashes so large an input). probe_one is 256
MiB over the first time; probe_all is cpus times 256 MiB over the wall time of the second. Both
are MB/s, 10^6 bytes a second, with one decimal. cache_entries is the number of files under
~/.cache/go-build, which the job's cache restore has just filled; a directory that is not there
holds none. goflags is GOFLAGS as the job's environment holds it, empty when unset. model is
lscpu's `Model name` (on arm64 /proc/cpuinfo names no model; where lscpu names more than one,
each distinct one, joined by `, `). It comes last because an x86_64 model holds spaces.

After that line `start` prints lscpu's output and the whole of /proc/cpuinfo between
`::group::go-cpu raw runner facts` and `::endgroup::`, so the log keeps the text the line was read
from, and copies both to D/runner/lscpu.txt and D/runner/cpuinfo.txt. It writes every fact of the
line to D/runner/runner.json, with `start_time`, the epoch seconds the measured window opens at,
and copies /proc/stat to D/runner/stat-start.txt; both are taken after the probe, so its CPU is not
the job's. Then it leaves behind a detached sampler (`sample`, a mode for `start` alone to run) in
a session of its own, with /dev/null for its stdin, stdout and stderr so it holds none of the
step's pipes open. Every 2 s the sampler appends `<epoch seconds>\t<the first line of /proc/stat>`
to D/runner/proc-stat.tsv, and its pid is in D/runner/sampler.pid. With --no-sampler, the
envoy-go-alone job's, whose tests run on a runner nothing else loads, `start` starts nothing, and
no process it starts outlives it.

`report` stops the sampler, copies /proc/stat to D/runner/stat-end.txt and prints:

    go-cpu job: <GITHUB_JOB> binaries=<n> nested=<n> runs=<n> user=<s> sys=<s> cpu=<s> wall=<s>
    go-cpu usage: busy=<s> user=<s> sys=<s> iowait=<s> steal=<s> idle=<s> new_cache_entries=<n> postgres_cpu=<s>

The job line reads every row D/*.json and puts each in the first of these classes it fits, so no
row counts twice: `nested`, a row whose parent is not null, which its parent's numbers already
hold; `outside`, a row whose cwd is absolute, outside the workspace; `list`, a row with an
argument `-test.list` or `-test.list=...`; `run`, a row whose binary does not end `.test`, a `go
run` program; and `test`, every other row. binaries is the number of test, list and outside rows,
and user, sys and wall the sums of their user_s, sys_s and wall_s; cpu is user plus sys, the job's
total CPU seconds in its binaries. nested and runs count those rows, which are in no sum. A row
file that cannot be read as a row is counted in a ` bad=<n>` at the end of the line.

The usage line is the first `cpu` line of stat-end.txt less that of stat-start.txt, in seconds
(ticks over SC_CLK_TCK), for the whole machine, all its CPUs added: of the fields user nice system
idle iowait irq softirq steal guest guest_nice, user is user+nice, sys is system+irq+softirq, idle,
iowait and steal are as named, and busy is user+nice+system+irq+softirq+steal; guest and
guest_nice are already inside user and nice, so they are added to nothing. new_cache_entries is
the files under ~/.cache/go-build now less start's cache_entries. postgres_cpu is the CPU seconds
of each `--postgres NAME=CONTAINER` added up: `docker exec <CONTAINER> cat /sys/fs/cgroup/cpu.stat`
(10 s at most), kept as D/runner/postgres-<NAME>.cpu.stat, gives the container's usage_usec. It is
NONE when no --postgres was given. Every seconds figure has two decimals.

With --no-rows, the envoy-go-checks job's, which runs no go test, `report` prints the usage line
alone, without new_cache_entries and postgres_cpu, and reads no row and no container.

The script only measures, so it never fails the job: each fact it cannot read prints UNKNOWN in
its place (postgres_cpu too, when any one container's usage_usec cannot be read), a file it cannot
write is left unwritten, and it exits 0. Only a usage error exits 2: an unknown argument, a
--postgres that is not NAME=CONTAINER, or no RUNNER_TEMP to put D under. It writes nothing to
$GITHUB_ENV or $GITHUB_PATH, for the reason disk_watch.py's header gives: a step that writes either
changes what ci_filter_coverage.py reads of its whole job.

Standard library only. Linux only: it reads /proc.
"""

import argparse
import contextlib
import hashlib
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time

UNKNOWN = "UNKNOWN"

# The probe's buffer: 256 MiB of a fixed repeating pattern, the same bytes on every runner, so two
# runners' rates differ by their CPUs alone.
PROBE_BYTES = 256 * 1024 * 1024
PROBE_PATTERN = bytes(range(256))

# How often the sampler reads /proc/stat. Each line costs one interpreter wake-up, and two seconds
# still shows which steps of a job kept the CPUs busy.
SAMPLE_SECONDS = 2.0

# The sampler gives up after this long on its own, so a job cancelled between `start` and `report`
# leaves nothing running behind it on a machine that is not thrown away after the job. GitHub ends
# every job at 6 hours, so no job the sampler watches outlasts it.
SAMPLER_MAX_SECONDS = 6 * 3600

# How long `report` waits for the sampler to go after SIGTERM before it reads /proc/stat anyway.
STOP_SECONDS = 2.0

# How long lscpu may take, and each docker exec, before the script gives up on it.
LSCPU_SECONDS = 10
DOCKER_SECONDS = 10

# A --postgres NAME, which names a file, and CONTAINER, which docker reads as an argument: Docker's
# own alphabet for container names and ids, so neither can leave D/runner or be read as an option.
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")

# This file's real path, which `start` runs the sampler from and `report` knows the sampler by.
SCRIPT = os.path.realpath(__file__)


def record_directory() -> str | None:
    """D, where the rows are, or None when RUNNER_TEMP is unset or empty."""
    runner_temp = os.environ.get("RUNNER_TEMP")
    if not runner_temp:
        return None
    return os.path.join(runner_temp, "go-test-events", "cpu")


def read_text(path: str) -> str | None:
    """The whole of a file as text, or None when it cannot be read."""
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return None


def save(path: str, text: str) -> None:
    """Write text to path, making its directory; a file that cannot be written is left unwritten,
    because the script only measures."""
    with contextlib.suppress(OSError):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(text)


def shown(value: object, spelled: str = "{}") -> str:
    """A fact as the printed lines spell it: UNKNOWN for one that could not be read."""
    return UNKNOWN if value is None else spelled.format(value)


def cpu_parts(cpuinfo: str) -> str:
    """Each distinct `CPU part` of /proc/cpuinfo with the number of CPUs that have it, in the order
    of first appearance; empty when it names none."""
    counts: dict[str, int] = {}
    for line in cpuinfo.splitlines():
        name, colon, value = line.partition(":")
        if colon and name.strip() == "CPU part":
            value = value.strip()
            counts[value] = counts.get(value, 0) + 1
    return ",".join(f"{value}:{count}" for value, count in counts.items())


def allowed_cpus() -> int | None:
    """The CPUs this process may run on, the number nproc prints."""
    try:
        return len(os.sched_getaffinity(0))
    except OSError:
        return None


def mem_total_kib() -> int | None:
    """MemTotal of /proc/meminfo, which the kernel gives in KiB and spells kB."""
    meminfo = read_text("/proc/meminfo")
    for line in (meminfo or "").splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[0] == "MemTotal:":
            with contextlib.suppress(ValueError):
                return int(fields[1])
    return None


def kernel_release() -> str | None:
    """The release uname -r prints."""
    try:
        return os.uname().release
    except OSError:
        return None


def run_lscpu() -> str | None:
    """lscpu's output, or None when it cannot be run or fails. It runs in the C locale, so its
    labels are the English ones the model is read by."""
    try:
        done = subprocess.run(
            ["lscpu"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env={**os.environ, "LC_ALL": "C"},
            timeout=LSCPU_SECONDS,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if done.returncode != 0:
        return None
    return done.stdout.decode("utf-8", errors="replace")


def model_name(lscpu: str) -> str | None:
    """lscpu's `Model name`. A machine of more than one kind of core gets one line per kind, so
    each distinct value is kept, joined by `, `."""
    names: list[str] = []
    for line in lscpu.splitlines():
        label, colon, value = line.partition(":")
        if colon and label.strip() == "Model name" and value.strip() not in names:
            names.append(value.strip())
    return ", ".join(names) if names else None


def go_build_cache_entries() -> int | None:
    """The files under ~/.cache/go-build: 0 when the directory is not there, which is a cache
    nothing has filled, and None when it is there and cannot be read whole."""
    root = os.path.join(os.path.expanduser("~"), ".cache", "go-build")
    if not os.path.lexists(root):
        return 0

    def unreadable(error: OSError) -> None:
        raise error

    count = 0
    try:
        for _, _, files in os.walk(root, onerror=unreadable):
            count += len(files)
    except OSError:
        return None
    return count


def probe(cpus: int | None) -> tuple[float | None, float | None]:
    """probe_one and probe_all in MB/s, rounded to one decimal, as the header gives them; None for
    a rate the machine would not run: either, with no room for the buffer, or probe_all, when it
    would not start every one of the cpus threads."""
    try:
        buffer = PROBE_PATTERN * (PROBE_BYTES // len(PROBE_PATTERN))
    except MemoryError:
        return None, None
    started = time.perf_counter()
    hashlib.sha256(buffer).digest()
    one = round(PROBE_BYTES / (time.perf_counter() - started) / 1e6, 1)
    every = None
    if cpus:
        # The threads wait on one event, so the wall time is of the hashing alone and not of
        # starting the threads one after another.
        go = threading.Event()

        def hash_once() -> None:
            go.wait()
            hashlib.sha256(buffer).digest()

        threads = []
        with contextlib.suppress(RuntimeError):
            # RuntimeError is a thread the machine would not start; the ones already started are
            # still let go and joined below, so none is left waiting.
            for _ in range(cpus):
                thread = threading.Thread(target=hash_once, daemon=True)
                thread.start()
                threads.append(thread)
        started = time.perf_counter()
        go.set()
        for thread in threads:
            thread.join()
        if len(threads) == cpus:
            every = round(cpus * PROBE_BYTES / (time.perf_counter() - started) / 1e6, 1)
    del buffer
    return one, every


def start_sampler(runner: str) -> None:
    """Start the sampler, detached, and write its pid to sampler.pid. A sampler that cannot be
    started leaves no pid, and the job's usage line is still read from the two copies of
    /proc/stat."""
    try:
        sampler = subprocess.Popen(
            [sys.executable, SCRIPT, "sample", "--directory", runner],
            # The sampler outlives this step, so it must hold none of the step's pipes: a child
            # still holding stdout keeps the runner waiting for an end of file that never comes.
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
            # Nor the step's directory.
            cwd="/",
        )
    except (OSError, subprocess.SubprocessError):
        return
    save(os.path.join(runner, "sampler.pid"), f"{sampler.pid}\n")


def start(sampler: bool) -> int:
    """Print the runner's facts and the raw text they were read from, keep both under D/runner,
    take /proc/stat as the job's start, and leave the sampler running unless told not to."""
    runner = os.path.join(record_directory(), "runner")
    cpuinfo = read_text("/proc/cpuinfo")
    lscpu = run_lscpu()
    cpus = allowed_cpus()
    probe_one, probe_all = probe(cpus)
    facts = {
        "part": None if cpuinfo is None else cpu_parts(cpuinfo),
        "cpus": cpus,
        "mem_kib": mem_total_kib(),
        "kernel": kernel_release(),
        "probe_one": probe_one,
        "probe_all": probe_all,
        "cache_entries": go_build_cache_entries(),
        "goflags": os.environ.get("GOFLAGS", ""),
        "model": None if lscpu is None else model_name(lscpu),
    }
    # The window the usage line measures opens here, after the probe, whose own CPU is no part of
    # the job's.
    facts["start_time"] = round(time.time(), 3)
    stat = read_text("/proc/stat")
    if stat is not None:
        save(os.path.join(runner, "stat-start.txt"), stat)
    if lscpu is not None:
        save(os.path.join(runner, "lscpu.txt"), lscpu)
    if cpuinfo is not None:
        save(os.path.join(runner, "cpuinfo.txt"), cpuinfo)
    save(os.path.join(runner, "runner.json"), json.dumps(facts) + "\n")
    if sampler:
        start_sampler(runner)

    print(
        f"go-cpu runner: part={shown(facts['part'])} cpus={shown(cpus)}"
        f" mem_kib={shown(facts['mem_kib'])} kernel={shown(facts['kernel'])}"
        f" probe_one={shown(probe_one, '{:.1f}')} probe_all={shown(probe_all, '{:.1f}')}"
        f" cache_entries={shown(facts['cache_entries'])} goflags=\"{facts['goflags']}\""
        f" model=\"{shown(facts['model'])}\""
    )
    print("::group::go-cpu raw runner facts")
    raw = (lscpu if lscpu is not None else "lscpu: UNKNOWN\n") + (
        cpuinfo if cpuinfo is not None else "/proc/cpuinfo: UNKNOWN\n"
    )
    # ::endgroup:: is a workflow command only at the start of a line.
    sys.stdout.write(raw if raw.endswith("\n") else raw + "\n")
    print("::endgroup::")
    return 0


def sample(runner: str) -> int:
    """The detached sampler: append the time and the first line of /proc/stat to proc-stat.tsv
    every SAMPLE_SECONDS until `report` stops it, its directory is gone, or SAMPLER_MAX_SECONDS
    have passed."""
    path = os.path.join(runner, "proc-stat.tsv")
    deadline = time.monotonic() + SAMPLER_MAX_SECONDS
    tick = time.monotonic()
    while tick < deadline:
        try:
            with open("/proc/stat", encoding="utf-8") as f:
                line = f.readline().rstrip("\n")
        except OSError:
            line = None
        if line:
            # SIGTERM waits while a line is written, so the file never ends in part of one.
            signal.pthread_sigmask(signal.SIG_BLOCK, [signal.SIGTERM])
            try:
                with open(path, "a", encoding="utf-8") as f:
                    f.write(f"{time.time():.3f}\t{line}\n")
            except FileNotFoundError:
                # The directory is gone, its job's files cleaned up: there is nothing left to
                # record into.
                return 0
            except OSError:
                pass
            signal.pthread_sigmask(signal.SIG_UNBLOCK, [signal.SIGTERM])
        # The next tick is the next due one that has not gone by, so a sampler held off the CPU
        # writes no burst of lines to catch up.
        tick = max(tick + SAMPLE_SECONDS, time.monotonic())
        time.sleep(max(0.0, tick - time.monotonic()))
    return 0


def is_sampler(pid: int) -> bool:
    """Whether pid is a sampler of this script that has not ended, so a pid the kernel has since
    given to another process is never signalled."""
    try:
        with open(f"/proc/{pid}/cmdline", "rb") as f:
            argv = f.read().split(b"\0")
    except OSError:
        return False
    # An ended process waiting to be reaped has an empty command line.
    return argv[1:3] == [os.fsencode(SCRIPT), b"sample"]


def stop_sampler(runner: str) -> None:
    """SIGTERM the sampler sampler.pid names, if it runs, and wait for it to go. No pid, or one
    that is gone, is no failure: the report measures, it does not police."""
    try:
        pid = int((read_text(os.path.join(runner, "sampler.pid")) or "").strip())
    except ValueError:
        return
    if pid <= 0 or not is_sampler(pid):
        return
    try:
        os.kill(pid, signal.SIGTERM)
    except OSError:
        return
    deadline = time.monotonic() + STOP_SECONDS
    while time.monotonic() < deadline and is_sampler(pid):
        time.sleep(0.05)


def first_cpu_ticks(text: str | None) -> list[int] | None:
    """user nice system idle iowait irq softirq steal of the first `cpu` line of a copy of
    /proc/stat, or None when there is none."""
    for line in (text or "").splitlines():
        fields = line.split()
        if fields and fields[0] == "cpu":
            try:
                ticks = [int(field) for field in fields[1:9]]
            except ValueError:
                return None
            return ticks if len(ticks) == 8 else None
    return None


def usage_sums(start_stat: str | None, end_stat: str | None) -> list[str]:
    """busy, user, sys, iowait, steal and idle, in seconds with two decimals, as the header gives
    them; UNKNOWN for each when either copy of /proc/stat cannot be read."""
    before, after = first_cpu_ticks(start_stat), first_cpu_ticks(end_stat)
    if before is None or after is None:
        return [UNKNOWN] * 6
    user, nice, system, idle, iowait, irq, softirq, steal = (b - a for a, b in zip(before, after))
    tick = os.sysconf("SC_CLK_TCK")
    return [
        f"{ticks / tick:.2f}"
        for ticks in (
            user + nice + system + irq + softirq + steal,
            user + nice,
            system + irq + softirq,
            iowait,
            steal,
            idle,
        )
    ]


def container_usage_usec(runner: str, name: str, container: str) -> int | None:
    """usage_usec of the container's own cgroup, read inside it, its cpu.stat kept as
    postgres-<name>.cpu.stat; None when docker cannot read it or it has no usage_usec."""
    try:
        done = subprocess.run(
            ["docker", "exec", container, "cat", "/sys/fs/cgroup/cpu.stat"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=DOCKER_SECONDS,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if done.returncode != 0:
        return None
    text = done.stdout.decode("utf-8", errors="replace")
    save(os.path.join(runner, f"postgres-{name}.cpu.stat"), text)
    for line in text.splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] == "usage_usec":
            with contextlib.suppress(ValueError):
                return int(fields[1])
    return None


def postgres_cpu(runner: str, postgres: list[tuple[str, str]]) -> str:
    """The CPU seconds of every --postgres container added up: NONE for none given, UNKNOWN when
    any one of them cannot be read. Every container is read, so each keeps its file."""
    if not postgres:
        return "NONE"
    usages = [container_usage_usec(runner, name, container) for name, container in postgres]
    if None in usages:
        return UNKNOWN
    return f"{sum(usages) / 1e6:.2f}"


def new_cache_entries(runner: str) -> str:
    """The files under ~/.cache/go-build now less those `start` counted."""
    try:
        with open(os.path.join(runner, "runner.json"), encoding="utf-8") as f:
            then = json.load(f).get("cache_entries")
    except (OSError, ValueError, AttributeError):
        then = None
    now = go_build_cache_entries()
    if not isinstance(then, int) or isinstance(then, bool) or now is None:
        return UNKNOWN
    return str(now - then)


def is_number(value: object) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def row_class(row: object) -> str | None:
    """The class of a row, as the header gives them, or None when it is no row this can read."""
    if not isinstance(row, dict):
        return None
    cwd, binary, args = row.get("cwd"), row.get("binary"), row.get("args")
    if not (
        isinstance(cwd, str)
        and isinstance(binary, str)
        and isinstance(args, list)
        and all(isinstance(arg, str) for arg in args)
        and all(is_number(row.get(name)) for name in ("user_s", "sys_s", "wall_s"))
    ):
        return None
    if row.get("parent") is not None:
        return "nested"
    if os.path.isabs(cwd):
        return "outside"
    if any(arg == "-test.list" or arg.startswith("-test.list=") for arg in args):
        return "list"
    if not binary.endswith(".test"):
        return "run"
    return "test"


def job_line(directory: str) -> str:
    """The `go-cpu job:` line of the rows in D."""
    try:
        names = sorted(os.listdir(directory))
    except OSError:
        names = []
    counted = nested = runs = bad = 0
    user = system = wall = 0.0
    for name in names:
        # The wrapper writes a row under a dot name and renames it into place.
        if name.startswith(".") or not name.endswith(".json"):
            continue
        path = os.path.join(directory, name)
        if not os.path.isfile(path):
            continue
        try:
            with open(path, encoding="utf-8") as f:
                row = json.load(f)
        except (OSError, ValueError):
            row = None
        kind = row_class(row)
        if kind is None:
            bad += 1
        elif kind == "nested":
            nested += 1
        elif kind == "run":
            runs += 1
        else:
            counted += 1
            user += row["user_s"]
            system += row["sys_s"]
            wall += row["wall_s"]
    line = (
        f"go-cpu job: {os.environ.get('GITHUB_JOB') or UNKNOWN} binaries={counted} nested={nested}"
        f" runs={runs} user={user:.2f} sys={system:.2f} cpu={user + system:.2f} wall={wall:.2f}"
    )
    return line + (f" bad={bad}" if bad else "")


def report(rows: bool, postgres: list[tuple[str, str]]) -> int:
    """Stop the sampler, take /proc/stat as the job's end, and print the job line, unless told
    there are no rows, and the usage line."""
    directory = record_directory()
    runner = os.path.join(directory, "runner")
    stop_sampler(runner)
    # The end is read before docker and the rows, whose reading is no part of the job.
    end_stat = read_text("/proc/stat")
    if end_stat is not None:
        save(os.path.join(runner, "stat-end.txt"), end_stat)
    sums = usage_sums(read_text(os.path.join(runner, "stat-start.txt")), end_stat)
    usage = "go-cpu usage: busy={} user={} sys={} iowait={} steal={} idle={}".format(*sums)
    if rows:
        cache = new_cache_entries(runner)
        usage += f" new_cache_entries={cache} postgres_cpu={postgres_cpu(runner, postgres)}"
        print(job_line(directory))
    print(usage)
    return 0


def postgres_pair(text: str) -> tuple[str, str]:
    """A --postgres NAME=CONTAINER."""
    name, equals, container = text.partition("=")
    if not (equals and NAME.fullmatch(name) and NAME.fullmatch(container)):
        raise argparse.ArgumentTypeError(
            f"{text!r} is not NAME=CONTAINER, each of letters, digits, _, . and -,"
            " a letter or a digit first"
        )
    return name, container


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="go-cpu-runner.py",
        description="Record the runner a CI job ran on and the CPU the whole job used.",
    )
    # `sample` is in the parser but out of the help: `start` starts it, and nothing else should.
    modes = parser.add_subparsers(dest="mode", required=True, metavar="{start,report}")
    starting = modes.add_parser("start", help="print the runner's facts and start the sampler")
    starting.add_argument("--no-sampler", action="store_true", help="start no sampler")
    reporting = modes.add_parser("report", help="stop the sampler and print the job's CPU")
    reporting.add_argument("--no-rows", action="store_true", help="print the usage line alone")
    reporting.add_argument(
        "--postgres",
        action="append",
        default=[],
        type=postgres_pair,
        metavar="NAME=CONTAINER",
        help="a Postgres container whose CPU to add to postgres_cpu",
    )
    sampling = modes.add_parser("sample")
    sampling.add_argument("--directory", required=True)

    args = parser.parse_args(argv)
    if args.mode == "sample":
        return sample(args.directory)
    if record_directory() is None:
        parser.error(
            "RUNNER_TEMP is not set, so there is no $RUNNER_TEMP/go-test-events/cpu to record into"
        )
    if args.mode == "start":
        return start(not args.no_sampler)
    return report(not args.no_rows, args.postgres)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
