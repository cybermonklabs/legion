#!/usr/bin/env python3
"""Record the wall-clock time at which each line of a pooled harness's log was written.

.github/scripts/harness-pool.sh runs a job's shell harnesses beside each other, each into a log
of its own, and the job's wait step prints every log when the last harness has ended. GitHub
stamps a job's log by the line, so every line of a pooled harness carries the time the wait
printed it, not the time the harness wrote it: the run of a case, and of the harness itself, is
nowhere in what GitHub keeps. The weekly test inventory reads those logs, and every one of its
3,782 shell rows has no seconds for that reason.

So the pool starts this beside each harness, on the harness's log, and it writes one record per
line to the stamp file:

    <epoch seconds with microseconds>\\t<the line's bytes without its newline>\\n

The text is the log's own bytes, so the stamp file holds the log as well as the times and the
log itself need not be kept beside it.

It polls the log for growth rather than reading the harness's output itself: the harness's
standard output and error stay the file the pool gave it, and nothing of this stands between the
harness and that file. Nothing here can reach the harness. (The pool exports PYTHONUNBUFFERED=1 to
each harness so that a Python program's lines reach the log when it prints them; that is the
pool's change, not this file's.) A line's time is the moment this read it, which is after the
line was written and normally within one poll (50 ms by default, HARNESS_POOL_LINE_POLL) of it:
the sleep between polls is a least time, not a most, so a loaded runner can make a read, and the
time it gives, later than that. Lines read by one poll share its time, which is right for lines
written in one burst. A last line the harness left with no newline carries the time its bytes
were last read, since nothing says when such a line ended.

The recorder signals the end by creating the stop file: this then reads the log to its end,
stamps that last line, and checks what it stamped against the log's own bytes, hash for hash. It
writes the number of bytes it read to the end file only when the two agree, so a log rewritten
behind this reader's back - truncated and written again to the same length, say - leaves no end
file rather than a record that claims to be the log and is not. The recorder writes the log's
size beside the end file, and a reader takes the record as complete only when those two numbers
agree too (.github/scripts/harness_case_times.py).

The recorder's pid and its start time are given here as arguments: a recorder that is gone, or a
pid another process now holds, ends this at the next poll without an end file, since the harness
it was stamping may still be running and the record is then incomplete. A harness whose job was
cancelled keeps whatever was stamped before the cancel, because every record is flushed as it is
read.

Usage: harness_line_times.py <log> <stamp file> <stop file> <end file> <recorder pid> <its start>
"""

import hashlib
import os
import sys
import time

# How often the log is read for new bytes. The pool's own poll is a separate knob: this one is
# the resolution of every time this file writes, and a case's seconds are a difference of two of
# them.
POLL_SECONDS = float(os.environ.get("HARNESS_POOL_LINE_POLL") or 0.05)

# What the log is read in at the end, to check what was stamped against the file itself.
CHUNK = 1 << 20


class Lines:
    """What has been stamped so far: the unfinished last line, when its bytes were last read,
    the bytes consumed, and the hash of everything stamped, which the end check compares with the
    log's own."""

    def __init__(self):
        self.rest = b""
        self.rest_at = 0.0
        self.total = 0
        self.digest = hashlib.blake2b()


def recorder_start(pid: str) -> str | None:
    """The start time (field 22 of /proc/<pid>/stat) of that process, or None when there is no
    such process. The pool reads it the same way: a pid with its start time names one process,
    where a pid alone could name a later one."""
    try:
        with open(f"/proc/{pid}/stat", "rb") as stat:
            fields = stat.read().rpartition(b") ")[2].split()
    except OSError:
        return None
    return fields[19].decode() if len(fields) > 19 else None


def stamp(out, state: Lines, now: float, line: bytes, newline: bool) -> None:
    """One record: the time, a tab, the line's own bytes, a newline. The hash follows the log,
    so it takes the line and the newline the log held, not this file's."""
    out.write(b"%.6f\t" % now)
    out.write(line)
    out.write(b"\n")
    state.digest.update(line)
    if newline:
        state.digest.update(b"\n")
    state.total += len(line) + (1 if newline else 0)


def drain(log, out, state: Lines) -> int:
    """Read every byte the log holds now and stamp each complete line of it. Returns how many
    records it wrote. The time is read after the bytes, so it is never earlier than the write
    that put them there, and each read's bytes are split once, so a burst of short lines costs
    the length of the burst and not its square."""
    written = 0
    while True:
        data = log.read()
        if not data:
            return written
        now = time.time()
        parts = (state.rest + data).split(b"\n")
        state.rest = parts.pop()
        state.rest_at = now
        for line in parts:
            stamp(out, state, now, line, True)
            written += 1


def log_digest(path: str) -> str:
    """The hash of the log as it stands, read in one pass."""
    digest = hashlib.blake2b()
    with open(path, "rb") as log:
        while True:
            data = log.read(CHUNK)
            if not data:
                return digest.hexdigest()
            digest.update(data)


def record(log_path: str, lines_path: str, stop_path: str, end_path: str, pid: str, want: str) -> int:
    if recorder_start(pid) != want:
        print(f"harness_line_times: the recorder {pid} was already gone", file=sys.stderr)
        return 1
    state = Lines()
    # The recorder creates the log before it starts this, so an open that fails is a fault worth
    # reporting rather than a race to wait out; the pool reads this script's output into the
    # entry's recorder log, which the wait prints.
    with open(log_path, "rb") as log, open(lines_path, "wb") as out:
        while True:
            if drain(log, out, state):
                out.flush()
            if os.path.exists(stop_path):
                break
            if recorder_start(pid) != want:
                # The recorder is gone and its harness may not be: whatever is stamped stays,
                # and no end file claims the record is whole.
                out.flush()
                return 1
            time.sleep(POLL_SECONDS)
        drain(log, out, state)
        if state.rest:
            stamp(out, state, state.rest_at, state.rest, False)
        out.flush()
    if state.digest.hexdigest() != log_digest(log_path):
        print(f"harness_line_times: {log_path} is not what was stamped from it, so the record is a part",
              file=sys.stderr)
        return 1
    # Written whole by a rename, so a reader never sees half a number.
    tmp = end_path + ".tmp"
    with open(tmp, "w") as end:
        end.write(f"{state.total}\n")
    os.rename(tmp, end_path)
    return 0


def main(argv: list[str]) -> int:
    if len(argv) != 7:
        print("harness_line_times.py: usage: harness_line_times.py <log> <stamp file> <stop file> "
              "<end file> <recorder pid> <its start time>", file=sys.stderr)
        return 2
    return record(argv[1], argv[2], argv[3], argv[4], argv[5], argv[6])


if __name__ == "__main__":
    sys.exit(main(sys.argv))
