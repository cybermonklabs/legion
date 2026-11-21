"""The readings check-download-retry.test.sh's real-tree case makes of a copy of this repository's
tree. The case runs it as `python3 .github/scripts/download_retry_real_tree.py <reading> <args…>`:

  rows <jobs>                         how many rows --jobs printed
  cover <tree> <jobs> <sites>         the workflow jobs that write their own whole-number
                                      `timeout-minutes`, hold a site --list prints and are not
                                      in --jobs, on one line
  shape <tree> <jobs> <shape> <copy>  rewrites the copy's workflows into the shape, and prints
                                      `lowered <path> <job>` for each job whose key it lowered
                                      and `split <path> <job>` for each derivation it split
  unnamed <done> <gate output>        the lowered jobs no refusal of their own key names, on
                                      one line

<jobs> and <sites> are what the gate's --jobs and --list print over <tree>, and <done> is what
`shape` printed; every line of them is tab-separated. A gate that refuses the tree prints its
problems there instead of rows, so a line is read as a row only where it has a row's fields:
round 35's mutation pass ended the harness with a traceback here, and no `Results:` line, under
62 mutants that made the gate refuse the tree (dispatch://INFRA-1545, round 35).

The block that prices a job's key is the gate's own answer, read from --jobs' sixth and seventh
columns, and never a walk of this program's. Review round 29 of dispatch://INFRA-1545 found that
this case re-implemented the gate's pairing walk; it reads the gate's answer instead, so the block
it reads a sum from is the block the gate holds the key to. What this program still reads for
itself is the derivation inside that block, whose sum the shapes lower the key below: that
reading is the case's oracle for the sum.
"""
import re
import sys
from pathlib import Path

import yaml

DERIVATION = re.compile(r"(\d+)\s*\+\s*(\d+)\s*=\s*(\d+)\s*s\s*->\s*(\d+)\s*min")
KEY = re.compile(r"^(\s*timeout-minutes:\s*)(\d+)(.*)$")
SHAPES = ("key", "arrows", "decoy-after", "decoy-before", "split", "neighbour", "more-figures")
NUMBER = re.compile(r"-?[0-9]+")


def is_comment(text):
    return text.lstrip().startswith("#")


def lead(text):
    return text[:len(text) - len(text.lstrip())]


def workflow_jobs(text):
    """Each job of a workflow, as id -> (its id's line, the line its value ends on, whether it
    writes its own `timeout-minutes` as a whole number), lines counted from 0. A key written as
    an expression is one the gate does not read, as its header says, so it is not counted."""
    root = yaml.compose(text)
    jobs = next(value for key, value in root.value if key.value == "jobs")
    return {key.value: (key.start_mark.line, value.end_mark.line,
                        isinstance(value, yaml.MappingNode)
                        and any(name.value == "timeout-minutes"
                                and isinstance(minutes, yaml.ScalarNode)
                                and re.fullmatch(r"[0-9]+", minutes.value) is not None
                                for name, minutes in value.value))
            for key, value in jobs.value}


def job_rows(jobs_file):
    """The rows --jobs printed, as (path, job, key line counted from 0, minutes, cost, block):
    its lines of eight tab-separated fields whose third, fourth and fifth are whole numbers.
    The block is the gate's pricing block for the key, its sixth and seventh fields, as (first
    line, last line) counted from 0, or None where the gate printed `-` for them."""
    for line in Path(jobs_file).read_text().splitlines():
        fields = line.split("\t")
        if len(fields) == 8 and all(NUMBER.fullmatch(field) for field in fields[2:5]):
            path, job, key, minutes, cost, first, last, _step = fields
            block = None
            if NUMBER.fullmatch(first) and NUMBER.fullmatch(last):
                block = (int(first) - 1, int(last) - 1)
            yield path, job, int(key) - 1, int(minutes), int(cost), block


def rows(jobs_file):
    print(sum(1 for _ in job_rows(jobs_file)))


def cover(tree, jobs_file, sites_file):
    listed = {(path, job) for path, job, *_ in job_rows(jobs_file)}
    sites: dict[str, set[int]] = {}
    for line in Path(sites_file).read_text().splitlines():
        # A site --list prints is four tab-separated fields: its path, line, verb and step.
        fields = line.split("\t")
        if len(fields) != 4 or not re.fullmatch(r"[0-9]+", fields[1]):
            continue
        path, number = fields[:2]
        if path.startswith(".github/workflows/"):
            sites.setdefault(path, set()).add(int(number) - 1)
    omitted = []
    for path, numbers in sorted(sites.items()):
        jobs = workflow_jobs(Path(tree, path).read_text())
        holders = set()
        for number in numbers:
            # The job a site is in: the last one whose id is at or above it and whose value
            # reaches it.
            held = [(start, job) for job, (start, end, _own) in jobs.items()
                    if start <= number <= end]
            if held:
                holders.add(max(held)[1])
        omitted += [f"{job} of {path}" for job in sorted(holders)
                    if jobs[job][2] and (path, job) not in listed]
    if omitted:
        print(", ".join(omitted))


def own_sum(block, minutes, cost):
    """The job's own sum, as the gate holds its key to it: the slowest retried step's cost, and
    where the block's last derivation whose arrow names the key adds up, its total and its
    measured term plus that cost."""
    stated = [match for match in DERIVATION.finditer(block)
              if int(match.group(4)) == minutes
              and int(match.group(1)) + int(match.group(2)) == int(match.group(3))]
    if not stated:
        return cost
    measured, total = int(stated[-1].group(1)), int(stated[-1].group(3))
    return max(cost, total, measured + cost)


def shape_file(lines, jobs, shape):
    """Rewrites one workflow's lines, in place, into the shape, and returns what it did, as
    (`lowered` or `split`, job) pairs. Each job comes with the block the gate's --jobs says
    prices its key."""
    ids = {job: start for job, (start, _end, _own) in workflow_jobs("\n".join(lines)).items()}
    plans = []
    for job, key, minutes, cost, block in jobs:
        text = " ".join(lines[index].lstrip().lstrip("#").strip()
                        for index in range(block[0], block[1] + 1)) if block else ""
        low = own_sum(text, minutes, cost) // 60
        if low < 1 or low >= minutes or (block is None and shape != "key"):
            continue
        # A neighbour's price needs a job above this one to be its neighbour.
        if shape in ("neighbour", "more-figures") and ids[job] == min(ids.values()):
            continue
        plans.append((job, key, minutes, low, block))

    # Every edit is keyed by the original line it applies at, and the file is rebuilt in one
    # pass: a line's new text, lines inserted before a line, and a block replaced whole.
    text_at: dict[int, str] = {}
    before: dict[int, list[str]] = {}
    replace: dict[int, tuple[int, list[str]]] = {}
    gone = set()
    if shape in ("neighbour", "more-figures"):
        gone = {index for _job, _key, _minutes, _low, (first, last) in plans
                for index in range(first, last + 1)}
    done = []
    for job, key, minutes, low, block in plans:
        if shape == "more-figures":
            # The last comment line above this job's key once its own block is gone. Where that
            # ends a block of the previous job, with YAML between it and this job's id, the block
            # is given arrows naming the lowered key, more of them than the keys left below it
            # before that id: no comment line stands between it and this key, so only the id ends
            # its reach, and a gate that let it run on would price this job's key with it.
            # Elsewhere a comment line ends the reach whatever the gate does with the id, so the
            # job proves nothing about the id and is left alone. The block need not have held an
            # arrow before: in round 35 none of this tree's blocks so placed held one, so a shape
            # asking for one lowered nothing, and the last arrow block before the id, which an
            # earlier draft of this case took, hid no key even from a gate whose pairing walk does
            # not stop at the id (dispatch://INFRA-1545, round 35).
            previous = max(start for start in ids.values() if start < ids[job])
            end = key - 1
            while end > previous and (end in gone or not is_comment(lines[end])):
                end -= 1
            if not previous < end < ids[job] - 1:
                continue
            keys = sum(1 for index in range(end + 1, ids[job]) if KEY.match(lines[index]))
            text_at[end] = (lines[end] + " The property case's floors: "
                            + " ".join([f"-> {low} min"] * (keys + 1)) + ".")
        text_at[key] = KEY.sub(lambda found: f"{found.group(1)}{low}{found.group(3)}", lines[key])
        done.append(("lowered", job))
        if shape == "key":
            continue
        first, last = block
        if shape == "neighbour":
            # The job's own price goes, and a step-depth comment stands directly over its id
            # with the lowered key's arrow, deeper than the id, so it prices no job.
            before.setdefault(ids[job], []).append(
                f"{lead(lines[ids[job]])}    # The neighbour step's own price -> {low} min.")
        if shape in ("neighbour", "more-figures"):
            replace[first] = (last, [])
            continue
        arrow = re.compile(rf"->\s*{minutes}\s*min")
        new = [arrow.sub(f"-> {low} min", lines[index]) for index in range(first, last + 1)]
        if shape == "decoy-after":
            new.append(f"{lead(lines[last])}# A decoy the property case adds: "
                       f"1 + 1 = 2 s -> {low + 500} min.")
        elif shape == "decoy-before":
            new.insert(0, f"{lead(lines[first])}# A decoy the property case adds: "
                          f"1 + 1 = 2 s -> {low} min.")
        elif shape == "split":
            # The derivation the gate reads, the last naming the key, broken after its `=`.
            sum_of = re.compile(rf"\d+\s*\+\s*\d+\s*=\s*(?=\d+\s*s\s*->\s*{low}\s*min)")
            for index in reversed(range(len(new))):
                found = list(sum_of.finditer(new[index]))
                if found:
                    cut = found[-1].end()
                    new[index:index + 1] = [new[index][:cut].rstrip(),
                                            f"{lead(new[index])}# {new[index][cut:].lstrip()}"]
                    done.append(("split", job))
                    break
        replace[first] = (last, new)

    out: list[str] = []
    index = 0
    while index < len(lines):
        out.extend(before.get(index, []))
        if index in replace:
            end, new = replace[index]
            out.extend(new)
            index = end + 1
            continue
        out.append(text_at.get(index, lines[index]))
        index += 1
    lines[:] = out
    return done


def reshape(tree, jobs_file, shape, copy):
    if shape not in SHAPES:
        raise SystemExit(f"no shape {shape!r}: the shapes are {', '.join(SHAPES)}")
    by_path: dict[str, list[tuple[str, int, int, int, tuple[int, int] | None]]] = {}
    for path, job, key, minutes, cost, block in job_rows(jobs_file):
        by_path.setdefault(path, []).append((job, key, minutes, cost, block))
    for path, jobs in sorted(by_path.items()):
        lines = Path(tree, path).read_text().split("\n")
        for kind, job in shape_file(lines, jobs, shape):
            print(f"{kind}\t{path}\t{job}")
        Path(copy, path).write_text("\n".join(lines))


def unnamed(done_file, output_file):
    output = Path(output_file).read_text().splitlines()
    missing = []
    for line in Path(done_file).read_text().splitlines():
        kind, path, job = line.split("\t")
        if kind == "lowered" and not any(
                refusal.startswith(f"::error file={path},")
                and (f"job '{job}' carries" in refusal or f"the key of job '{job}'," in refusal)
                for refusal in output):
            missing.append(f"{job} of {path}")
    if missing:
        print(", ".join(missing))


{"rows": rows, "cover": cover, "shape": reshape, "unnamed": unnamed}[sys.argv[1]](*sys.argv[2:])
