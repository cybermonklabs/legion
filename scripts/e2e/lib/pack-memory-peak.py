#!/usr/bin/env python3
"""pack-memory-peak.py: run one command under GNU time -v and sample its process tree's memory.

usage:
  pack-memory-peak.py --label L --time-bin T --out-dir D --results F [--timeout S] [--inherit]
                      [--daemon PID] [--env NAME=VALUE]... [--unset NAME]... -- COMMAND [ARG]...

The sampler of scripts/e2e/pack-memory-probe.sh, which the pack-memory Job runs inside the worker
image: scripts/e2e/pack-memory-job.sh writes this file into the Job's command beside the probe,
since a Job holds no checkout to read it from.

It runs COMMAND under GNU time -v (T, whose report goes to D/L.time) and, every 0.1 s until that
process ends, reads /proc/<pid>/smaps of every process of the tree under it. gVisor's
/proc/<pid>/status carries no RssAnon or RssFile, so the split of a sample comes from smaps alone:

  - total: the Rss of every mapping of every process of the tree, summed at one instant;
  - pack: the Rss of the mappings whose path holds /objects/pack/ (a pack, its index, a
    multi-pack-index);
  - anonymous: the Anonymous pages of every mapping;
  - other file: the Rss of every other mapping of a file.

The figures it reports are those of one sample, the one whose total was the largest, so the pack
Rss, the anonymous pages and the counts beside the total are the split of that very total, never
maxima read at different instants. In that sample a pack mapping counts once per mapping
(pack_mappings; pack_mappings_resident counts those with a resident page) and its file once per
path, however many mappings or processes share it (pack_files). A peak shorter than one interval
can fall between two samples; GNU time's figure, the largest resident set of any one process of
the tree, is the one that sees it, which is why the probe bounds a row by both.

The tree is stopped while a sample reads it, so a sample is one instant: the sampler lists the
tree, sends each of its processes SIGSTOP, waits up to 50 ms for every thread of each to reach the
stop (state T in /proc/<pid>/task/<tid>/stat, or gone), lists the tree again for a process forked
meanwhile and stops that too, reads every smaps and the cgroup charge, and then sends SIGCONT to
every process it stopped: always, after a read that failed too, and also when this process gets
SIGTERM, SIGINT or SIGHUP or exits. One smaps read walks hundreds of mappings over milliseconds,
and a jj that drops one chunk of a pack and maps the next thousands of times a second, left
running while it is read, is counted holding both, more than any instant held. A process that
does not reach the stop in time is read anyway and its sample counted
(samples_not_stopped_in_time; largest_sample_stopped_in_time says it of the sample the figures
come from). It never stops itself, its parent or the --daemon tree, which it only reads, and a
process that something else had stopped it leaves as it found it. It reports the samples it
stopped the tree in (samples_stopped) and how long the tree was stopped in each, from the first
SIGSTOP to the last SIGCONT: their sum, median and longest (stop_ms_total, stop_ms_median,
stop_ms_max). The wall time (wall_s) is the time COMMAND ran, its stops included. A SIGKILL of
this process, which no handler sees, leaves a tree it had stopped stopped.

The tree runs in a process group that is never orphaned while it runs, since gVisor hangs up a
group that is orphaned while a member is stopped (SIGHUP, then SIGCONT, to every member). This
process leads that group, its own parent outside it, and is the tree's child subreaper, so every
orphan of the tree, such as the daemon `git maintenance run --auto --detach` leaves behind each
`git fetch`, is reparented inside the group: gVisor's count of a group's members whose parent is
outside it is not updated when a member is reparented. With GNU time leading a group of its own
beside a sampler in the probe's group instead, four of five `jj git fetch` rows of one octocat run
were hung up at their one stopped sample (exit 129, no GNU time report). Where its parent started
it in a group of its own, as provisioning's procgroup does, it keeps that group, so the parent's
stop of the group still reaches the whole tree. A process that leads a session of its own starts
GNU time in a group of its own instead, whose orphans go to a reaper in another session.

Beside that sample it reports, over every sample, the largest pack Rss, anonymous memory and
other-file Rss (max_pack_kib, max_anon_kib, max_other_file_kib), each its own largest and not one
instant's, which R10's pack and derived figures are made of; and, for each path under
/objects/pack/, the most mappings of it one sample held, across every process of the tree
(pack_file_mappings). The largest .pack file among them by size is named with its size and that
count (largest_pack_file, largest_pack_file_bytes, largest_pack_file_mappings, 0 when no sample
held a pack file): a build that maps a pack whole maps it once for each object store that opens
it, so that count is the command's number of stores. It reports each process of the tree too
(processes), largest first: its pid, the first three words of its cmdline when it was read (a
process that execs is a second entry), and, at the sample where its own total was largest, that
total and its split: the Rss of its mappings of files ending in .pack and in .idx, the Rss of
every other file mapping, and its Anonymous pages.

Beside the tree it reads the pod's cgroup memory charge, cgroup v1's
/sys/fs/cgroup/memory/memory.usage_in_bytes or, where that does not read, cgroup v2's
/sys/fs/cgroup/memory.current (cgroup_file): once just before COMMAND starts (cgroup_baseline_kib)
and at every sample, right after the tree's. It reports the largest charge a sample read
(cgroup_peak_kib), that peak less the baseline (cgroup_peak_less_baseline_kib), which is the
pod's charge of the command, and the charge at the sample of the tree's largest total less the
baseline (cgroup_at_largest_sample_kib); each is null when no sample was taken and "unreadable"
when neither file reads. Under gVisor a file that two processes map is charged to the cgroup
once, where the tree's total counts it in each, which is why the charge differs from the total.
The charge is the whole pod's, so it also holds what a process outside the tree did meanwhile:
--daemon PID samples the tree under PID at every sample too, a server COMMAND talks to that was
started outside its tree, and reports that tree's largest total (daemon_tree_peak_kib) beside
its pid (daemon_pid).

Without --inherit, COMMAND's stdout goes to D/L.out and its stderr to D/L.err, and the RESULT line
(`RESULT ` then one JSON object) is printed on stdout. With --inherit, COMMAND keeps this
process's stdin, stdout and stderr, so whatever runs this reads COMMAND's own output and nothing
else, and the RESULT goes to F alone. Either way the JSON object is appended to F, one line per
command. --env and --unset change COMMAND's environment, and the RESULT names both beside the
argv it ran.

This process exits with COMMAND's status, 128 plus the signal for a COMMAND a signal ended, and
124 for a COMMAND it killed, whole tree, at --timeout. A usage error exits 2 before anything runs.
"""

import atexit
import ctypes
import json
import os
import re
import signal
import statistics
import subprocess
import sys
import time

MAPPING = re.compile(r"^[0-9a-f]+-[0-9a-f]+ ")
PACK = "/objects/pack/"
INTERVAL = 0.1
TAIL = 400
# The pod's cgroup memory charge: cgroup v1's file, then cgroup v2's.
CGROUP_FILES = ("/sys/fs/cgroup/memory/memory.usage_in_bytes", "/sys/fs/cgroup/memory.current")
CGROUP_FIGURES = ("cgroup_baseline_kib", "cgroup_peak_kib", "cgroup_peak_less_baseline_kib", "cgroup_at_largest_sample_kib")
# How long a sample waits for every thread of the tree to reach its SIGSTOP, how often it looks,
# and how many times it lists the tree for a process forked while the rest was being stopped.
STOP_BOUND = 0.05
STOP_POLL = 0.001
STOP_ROUNDS = 3
# The processes stop_tree stopped and resume has not continued yet.
STOPPED = []
PR_SET_CHILD_SUBREAPER = 36


def child_subreaper():
    """Make this process the child subreaper of every process it starts, so an orphan of the tree
    is reparented to it rather than to init (prctl(2), PR_SET_CHILD_SUBREAPER)."""
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
        error = ctypes.get_errno()
        raise OSError(error, f"prctl(PR_SET_CHILD_SUBREAPER): {os.strerror(error)}")


def refuse(message):
    sys.stderr.write(f"pack-memory-peak.py: {message}\n")
    sys.stderr.write(__doc__.split("\n\n", 2)[1] + "\n")
    sys.exit(2)


def parse(argv):
    opts = {"timeout": None, "inherit": False, "daemon": None, "env": {}, "unset": []}
    valued = {"--label": "label", "--time-bin": "time_bin", "--out-dir": "out_dir", "--results": "results"}
    while argv:
        arg = argv.pop(0)
        if arg == "--":
            break
        if arg == "--inherit":
            opts["inherit"] = True
            continue
        if not argv:
            refuse(f"{arg} takes a value" if arg.startswith("--") else f"unknown argument {arg}")
        value = argv.pop(0)
        if arg in valued:
            opts[valued[arg]] = value
        elif arg == "--timeout":
            try:
                opts["timeout"] = float(value)
            except ValueError:
                refuse(f"--timeout {value} is no number of seconds")
        elif arg == "--daemon":
            if not value.isdigit():
                refuse(f"--daemon {value} is no process id")
            opts["daemon"] = int(value)
        elif arg == "--env":
            name, sep, setting = value.partition("=")
            if not sep or not name:
                refuse(f"--env {value} is no NAME=VALUE")
            opts["env"][name] = setting
        elif arg == "--unset":
            opts["unset"].append(value)
        else:
            refuse(f"unknown option {arg}")
    else:
        refuse("no -- before the command")
    for key in valued.values():
        if not opts.get(key):
            refuse(f"--{key.replace('_', '-')} is required")
    if not argv:
        refuse("no command after --")
    if os.sep in opts["label"] or opts["label"].startswith("."):
        refuse(f"the label {opts['label']} names a file of its own, so it holds no / and starts with no .")
    return opts, argv


def children():
    """Each process's children, read from each process's parent in /proc/<pid>/stat."""
    found = {}
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/stat") as f:
                stat = f.read()
            parent = int(stat.rsplit(")", 1)[1].split()[1])
        except (OSError, IndexError, ValueError):
            continue
        found.setdefault(parent, []).append(int(entry))
    return found


def tree(root):
    """root and every process below it, by each process's parent as it stands now."""
    kids = children()
    found, stack = [], [root]
    while stack:
        pid = stack.pop()
        found.append(pid)
        stack.extend(kids.get(pid, []))
    return found


def state(path):
    """The state letter a /proc/<pid>/stat or /proc/<pid>/task/<tid>/stat holds, None when gone."""
    try:
        with open(path) as f:
            return f.read().rsplit(")", 1)[1].split()[0]
    except (OSError, IndexError):
        return None


def halted(pid):
    """Whether every thread of pid is stopped, in a tracing stop, a zombie or gone."""
    try:
        threads = os.listdir(f"/proc/{pid}/task")
    except OSError:
        return True
    return all(state(f"/proc/{pid}/task/{tid}/stat") in (None, "T", "t", "Z", "X") for tid in threads)


def stop_tree(root, spared):
    """Stop the tree under root for one sample, sparing the pids in spared. Each round lists the
    tree, sends SIGSTOP to each process it had not listed, and waits up to STOP_BOUND for every
    thread of each to halt; a round that lists no new process ends it, and a process still new
    after STOP_ROUNDS rounds is read unstopped. Answers the processes to read, those that did not
    halt in time or were never stopped, and the monotonic time of the first SIGSTOP (None when it
    sent none)."""
    listed, late, first = [], [], None
    for _ in range(STOP_ROUNDS):
        new = [pid for pid in tree(root) if pid not in listed and pid not in spared]
        if not new:
            break
        listed.extend(new)
        sent = []
        for pid in new:
            if state(f"/proc/{pid}/stat") in ("T", "t"):
                continue  # something else stopped it, and only that continues it
            STOPPED.append(pid)  # before the signal: a handler that runs between them continues it
            try:
                os.kill(pid, signal.SIGSTOP)
            except OSError:
                continue
            if first is None:
                first = time.monotonic()
            sent.append(pid)
        deadline = time.monotonic() + STOP_BOUND
        while True:
            sent = [pid for pid in sent if not halted(pid)]
            if not sent or time.monotonic() >= deadline:
                break
            time.sleep(STOP_POLL)
        late.extend(sent)
    else:
        unstopped = [pid for pid in tree(root) if pid not in listed and pid not in spared]
        listed.extend(unstopped)
        late.extend(unstopped)
    return listed, late, first


def resume():
    """SIGCONT every process stop_tree stopped. The list is cleared only once each was sent one, so
    a signal handler that runs midway sends every one again rather than skipping one."""
    for pid in list(STOPPED):
        try:
            os.kill(pid, signal.SIGCONT)
        except OSError:
            pass
    STOPPED.clear()


def leave(signum, frame):
    """SIGTERM, SIGINT or SIGHUP: continue every stopped process, then end as that signal ends a
    process, as this one did before it stopped any."""
    resume()
    signal.signal(signum, signal.SIG_DFL)
    os.kill(os.getpid(), signum)
    os._exit(128 + signum)


def empty():
    """The record of an instant that holds nothing: every total and count zero."""
    return {"total": 0, "pack": 0, "anon": 0, "other": 0, "mappings": 0, "resident": 0, "files": 0}


def split_of(path):
    """The part of one process's split a mapping's Rss goes to: .pack, .idx, other file or none."""
    clean = path.removesuffix(" (deleted)")
    if clean.endswith(".pack"):
        return "pack"
    if clean.endswith(".idx"):
        return "idx"
    if clean and not clean.startswith("["):
        return "other"
    return None


def name_of(pid):
    """The first three words of the process's cmdline, or [comm] when it has none; None when gone."""
    try:
        with open(f"/proc/{pid}/cmdline", "rb") as f:
            words = [w.decode("utf-8", "replace") for w in f.read().split(b"\0") if w]
        if words:
            return " ".join(words[:3])
        with open(f"/proc/{pid}/stat") as f:
            return "[" + f.read().split("(", 1)[1].rsplit(")", 1)[0] + "]"
    except (OSError, IndexError):
        return None


def sample(pids):
    """One instant of the processes pids: their totals, the mappings of each pack path, and each
    process's own split (total, .pack, .idx, other file and anonymous), for those read whole."""
    totals = empty()
    per_file = {}
    per_pid = {}
    for pid in pids:
        own = {"total": 0, "pack": 0, "idx": 0, "other": 0, "anon": 0}
        try:
            with open(f"/proc/{pid}/smaps") as f:
                path = ""
                part = None
                for line in f:
                    if MAPPING.match(line):
                        parts = line.split(None, 5)
                        path = parts[5].strip() if len(parts) > 5 else ""
                        part = split_of(path)
                        if PACK in path:
                            totals["mappings"] += 1
                            per_file[path] = per_file.get(path, 0) + 1
                    elif line.startswith("Rss:"):
                        kib = int(line.split()[1])
                        totals["total"] += kib
                        own["total"] += kib
                        if part:
                            own[part] += kib
                        if PACK in path:
                            totals["pack"] += kib
                            if kib:
                                totals["resident"] += 1
                        elif path and not path.startswith("["):
                            totals["other"] += kib
                    elif line.startswith("Anonymous:"):
                        kib = int(line.split()[1])
                        totals["anon"] += kib
                        own["anon"] += kib
        except (OSError, IndexError, ValueError):
            # The process ended while it was read; what it held is gone from this instant.
            continue
        per_pid[pid] = own
    totals["files"] = len(per_file)
    return totals, per_file, per_pid


def size(path):
    """The size of the file a mapping names, None when it is gone or unreadable."""
    try:
        return os.stat(path.removesuffix(" (deleted)")).st_size
    except OSError:
        return None


def cgroup_file():
    """The first of CGROUP_FILES that reads as a number of bytes, None when neither does."""
    for path in CGROUP_FILES:
        if charge(path) is not None:
            return path
    return None


def charge(path):
    """The pod's cgroup memory charge path holds, in KiB; None when it does not read."""
    try:
        with open(path) as f:
            return int(f.read().strip()) // 1024
    except (OSError, ValueError):
        return None


def less(kib, baseline):
    """kib less the baseline, None when either is missing."""
    return None if kib is None or baseline is None else kib - baseline


def kill_tree(root):
    for pid in reversed(tree(root)):
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass


def gnu_time_report(path):
    """GNU time's maximum resident set (KiB), exit status and terminating signal, as -v wrote them."""
    maxrss = status = sig = None
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if line.startswith("Maximum resident set size (kbytes):"):
                    maxrss = int(line.rsplit(":", 1)[1])
                elif line.startswith("Exit status:"):
                    status = int(line.rsplit(":", 1)[1])
                elif line.startswith("Command terminated by signal"):
                    sig = int(line.rsplit(" ", 1)[1])
    except (OSError, ValueError):
        pass
    return maxrss, status, sig


def tail(path):
    try:
        with open(path, "rb") as f:
            body = f.read()
    except OSError:
        return 0, ""
    return len(body), body[-TAIL:].decode("utf-8", "replace")


def main():
    opts, command = parse(sys.argv[1:])
    label, out_dir = opts["label"], opts["out_dir"]
    env = dict(os.environ)
    for name in opts["unset"]:
        env.pop(name, None)
    env.update(opts["env"])
    time_file = os.path.join(out_dir, label + ".time")
    out_file = os.path.join(out_dir, label + ".out")
    err_file = os.path.join(out_dir, label + ".err")
    os.makedirs(out_dir, exist_ok=True)

    streams = {}
    if not opts["inherit"]:
        streams = {"stdout": open(out_file, "wb"), "stderr": open(err_file, "wb")}
    # A stop of the tree must always be undone: on these signals and at exit as well as after each
    # read. A signal that was ignored when this started stays ignored, as it was before.
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        if signal.getsignal(signum) is not signal.SIG_IGN:
            signal.signal(signum, leave)
    atexit.register(resume)
    # The tree's process group must never become orphaned while it runs (the docstring says why):
    # this process leads it, its own parent outside it, and is the tree's child subreaper, so an
    # orphan of the tree is reparented inside the group, and that orphan leaving the group counts
    # nothing. A session leader cannot lead a new group, so there GNU time leads one instead.
    session_leader = os.getsid(0) == os.getpid()
    if not session_leader:
        if os.getpgrp() != os.getpid():
            os.setpgid(0, 0)
        child_subreaper()
    never = {os.getpid(), os.getppid()}
    charged = cgroup_file()
    baseline = None if charged is None else charge(charged)
    started = time.monotonic()
    proc = subprocess.Popen([opts["time_bin"], "-v", "-o", time_file] + command, env=env,
                            process_group=0 if session_leader else None, **streams)
    for stream in streams.values():
        stream.close()

    largest = None
    samples = 0
    stops = []
    late_samples = 0
    charge_peak = None
    daemon_peak = 0
    timed_out = False
    peaks = {"pack": 0, "anon": 0, "other": 0}
    file_mappings = {}
    file_sizes = {}
    # Each process of the tree, by pid and the name its cmdline gave it then (an exec renames it),
    # with its own split at the sample where its own total was largest.
    processes = {}
    while proc.poll() is None:
        daemon_pids = [] if opts["daemon"] is None else tree(opts["daemon"])
        try:
            pids, late, first = stop_tree(proc.pid, never | set(daemon_pids))
            now, per_file, per_pid = sample(pids)
            now["charge"] = None if charged is None else charge(charged)
            names = {pid: name_of(pid) for pid in per_pid}
        finally:
            resume()
        if first is not None:
            stops.append((time.monotonic() - first) * 1000)
        now["stopped_in_time"] = not late
        late_samples += 1 if late else 0
        samples += 1
        if now["charge"] is not None and (charge_peak is None or now["charge"] > charge_peak):
            charge_peak = now["charge"]
        if daemon_pids:
            daemon_peak = max(daemon_peak, sample(daemon_pids)[0]["total"])
        if largest is None or now["total"] > largest["total"]:
            largest = now
            largest["at"] = time.monotonic() - started
        for key in peaks:
            peaks[key] = max(peaks[key], now[key])
        for path, count in per_file.items():
            file_mappings[path] = max(file_mappings.get(path, 0), count)
            if path not in file_sizes:
                file_sizes[path] = size(path)
        at = round(time.monotonic() - started, 1)
        for pid, own in per_pid.items():
            key = (pid, names[pid])
            if key not in processes or own["total"] > processes[key]["total_kib"]:
                processes[key] = {"pid": pid, "name": names[pid], "at_s": at, "total_kib": own["total"],
                                  "pack_kib": own["pack"], "idx_kib": own["idx"],
                                  "other_file_kib": own["other"], "anon_kib": own["anon"]}
        if opts["timeout"] is not None and not timed_out and time.monotonic() - started > opts["timeout"]:
            timed_out = True
            kill_tree(proc.pid)
        time.sleep(INTERVAL)
    wall = time.monotonic() - started
    if largest is None:
        largest = {**empty(), "at": None, "charge": None, "stopped_in_time": None}
    packs = [path for path in file_mappings if path.removesuffix(" (deleted)").endswith(".pack")]
    largest_pack = max(packs, key=lambda path: file_sizes[path] or 0, default=None)

    maxrss, status, sig = gnu_time_report(time_file)
    if timed_out:
        code = 124
    elif sig is not None:
        code = 128 + sig
    elif status is not None:
        code = status
    else:
        code = proc.returncode if proc.returncode >= 0 else 128 - proc.returncode

    result = {
        "label": label,
        "argv": command,
        "cwd": os.getcwd(),
        "env_set": opts["env"],
        "env_unset": opts["unset"],
        "exit": code,
        "signal": sig,
        "timed_out": timed_out,
        "wall_s": round(wall, 1),
        "maxrss_kib": maxrss,
        "sampled_total_kib": largest["total"],
        "sampled_pack_kib": largest["pack"],
        "sampled_anon_kib": largest["anon"],
        "sampled_other_file_kib": largest["other"],
        "pack_mappings": largest["mappings"],
        "pack_mappings_resident": largest["resident"],
        "pack_files": largest["files"],
        "largest_sample_at_s": None if largest["at"] is None else round(largest["at"], 1),
        "samples": samples,
        "samples_stopped": len(stops),
        "samples_not_stopped_in_time": late_samples,
        "largest_sample_stopped_in_time": largest["stopped_in_time"],
        "stop_ms_total": round(sum(stops), 1),
        "stop_ms_median": round(statistics.median(stops), 1) if stops else None,
        "stop_ms_max": round(max(stops), 1) if stops else None,
        "max_pack_kib": peaks["pack"],
        "max_anon_kib": peaks["anon"],
        "max_other_file_kib": peaks["other"],
        "pack_file_mappings": file_mappings,
        "largest_pack_file": largest_pack,
        "largest_pack_file_bytes": None if largest_pack is None else file_sizes[largest_pack],
        "largest_pack_file_mappings": 0 if largest_pack is None else file_mappings[largest_pack],
        "cgroup_file": charged,
        "cgroup_baseline_kib": baseline,
        "cgroup_peak_kib": charge_peak,
        "cgroup_peak_less_baseline_kib": less(charge_peak, baseline),
        "cgroup_at_largest_sample_kib": less(largest["charge"], baseline),
        "processes": sorted(processes.values(), key=lambda p: (-p["total_kib"], p["pid"])),
    }
    if charged is None:
        result.update(dict.fromkeys(CGROUP_FIGURES, "unreadable"))
    if opts["daemon"] is not None:
        result["daemon_pid"] = opts["daemon"]
        result["daemon_tree_peak_kib"] = daemon_peak
    if not opts["inherit"]:
        result["stdout_bytes"], result["stdout_tail"] = tail(out_file)
        result["stderr_bytes"], result["stderr_tail"] = tail(err_file)
    line = json.dumps(result)
    try:
        with open(opts["results"], "a") as f:
            f.write(line + "\n")
    except OSError as err:
        sys.stderr.write(f"pack-memory-peak.py: the result of {label} could not be appended to {opts['results']}: {err}\n")
    if not opts["inherit"]:
        print("RESULT " + line, flush=True)
    sys.exit(code if 0 <= code <= 255 else 1)


if __name__ == "__main__":
    main()
