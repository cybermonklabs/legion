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

Beside that sample it reports, over every sample, the largest pack Rss, anonymous memory and
other-file Rss (max_pack_kib, max_anon_kib, max_other_file_kib), each its own largest and not one
instant's, which R10's pack figure is made of; and, for each path under /objects/pack/, the most
mappings of it one sample held, across every process of the tree (pack_file_mappings). The largest
.pack file among them by size is named with its size and that count (largest_pack_file,
largest_pack_file_bytes, largest_pack_file_mappings, 0 when no sample held a pack file): a build
that maps a pack whole maps it once for each object store that opens it, so that count is the
command's number of stores.

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

import json
import os
import re
import signal
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


def tree(root):
    """root and every process below it, read from each process's parent in /proc/<pid>/stat."""
    children = {}
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/stat") as f:
                stat = f.read()
            parent = int(stat.rsplit(")", 1)[1].split()[1])
        except (OSError, IndexError, ValueError):
            continue
        children.setdefault(parent, []).append(int(entry))
    found, stack = [], [root]
    while stack:
        pid = stack.pop()
        found.append(pid)
        stack.extend(children.get(pid, []))
    return found


def empty():
    """The record of an instant that holds nothing: every total and count zero."""
    return {"total": 0, "pack": 0, "anon": 0, "other": 0, "mappings": 0, "resident": 0, "files": 0}


def sample(root):
    """One instant of the tree under root: its totals, and the mappings of each pack path."""
    totals = empty()
    per_file = {}
    for pid in tree(root):
        try:
            with open(f"/proc/{pid}/smaps") as f:
                path = ""
                for line in f:
                    if MAPPING.match(line):
                        parts = line.split(None, 5)
                        path = parts[5].strip() if len(parts) > 5 else ""
                        if PACK in path:
                            totals["mappings"] += 1
                            per_file[path] = per_file.get(path, 0) + 1
                    elif line.startswith("Rss:"):
                        kib = int(line.split()[1])
                        totals["total"] += kib
                        if PACK in path:
                            totals["pack"] += kib
                            if kib:
                                totals["resident"] += 1
                        elif path and not path.startswith("["):
                            totals["other"] += kib
                    elif line.startswith("Anonymous:"):
                        totals["anon"] += int(line.split()[1])
        except (OSError, IndexError, ValueError):
            # The process ended while it was read; what it held is gone from this instant.
            continue
    totals["files"] = len(per_file)
    return totals, per_file


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
    charged = cgroup_file()
    baseline = None if charged is None else charge(charged)
    started = time.monotonic()
    proc = subprocess.Popen([opts["time_bin"], "-v", "-o", time_file] + command, env=env, **streams)
    for stream in streams.values():
        stream.close()

    largest = None
    samples = 0
    charge_peak = None
    daemon_peak = 0
    timed_out = False
    peaks = {"pack": 0, "anon": 0, "other": 0}
    file_mappings = {}
    file_sizes = {}
    while proc.poll() is None:
        now, per_file = sample(proc.pid)
        now["charge"] = None if charged is None else charge(charged)
        samples += 1
        if now["charge"] is not None and (charge_peak is None or now["charge"] > charge_peak):
            charge_peak = now["charge"]
        if opts["daemon"] is not None:
            daemon_peak = max(daemon_peak, sample(opts["daemon"])[0]["total"])
        if largest is None or now["total"] > largest["total"]:
            largest = now
            largest["at"] = time.monotonic() - started
        for key in peaks:
            peaks[key] = max(peaks[key], now[key])
        for path, count in per_file.items():
            file_mappings[path] = max(file_mappings.get(path, 0), count)
            if path not in file_sizes:
                file_sizes[path] = size(path)
        if opts["timeout"] is not None and not timed_out and time.monotonic() - started > opts["timeout"]:
            timed_out = True
            kill_tree(proc.pid)
        time.sleep(INTERVAL)
    wall = time.monotonic() - started
    if largest is None:
        largest = {**empty(), "at": None, "charge": None}
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
