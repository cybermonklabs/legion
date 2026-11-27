#!/usr/bin/env python3
"""Fills job.yaml from the environment and prints the Job.

    python3 render-job.py < job.yaml > "$TMPDIR/job.yaml"

Each ${NAME} of job.yaml is the variable NAME. job.yaml's header names them and says what each
means. A variable it marks optional drops the line that names it when it is unset or empty; every
other variable is required, and the render refuses when one is missing. The render also computes
what follows from the sizes: the driver container's CPU (the pod's CPU less the server's and
Postgres's), and the CPU-hours and memory GiB-hours of the Job's cost annotations (one attempt is
at most two hours, the pod's deadline). It names no compute class, runtime class or machine size:
the deployment that renders the Job chooses them.
"""
import os
import re
import sys

REQUIRED = (
    "COMMIT", "BASE", "MODE", "SCALE", "TEN_TIMES", "OMP_SESSION", "OMP_SESSION_TITLE", "LABEL_DOMAIN",
    "COMPUTE_CLASS", "SERVER_CPU", "SERVER_MEMORY", "PG_CPU", "PG_MEMORY", "POD_CPU", "DRIVER_MEMORY",
    "SCRATCH", "COST_USD",
)
OPTIONAL = ("RUNTIME_CLASS", "GATING_CLASS", "PAINT_RUNS")
ATTEMPT_HOURS = 2


def count(name, value, unit="CPUs"):
    if not re.fullmatch(r"[1-9][0-9]*", value):
        sys.exit(f"render-job.py: {name} is a whole number of {unit} from 1, not {value!r}")
    return int(value)


def gib(name, value):
    if not re.fullmatch(r"[1-9][0-9]*Gi", value):
        sys.exit(f"render-job.py: {name} is a whole number of Gi, such as 24Gi, not {value!r}")
    return int(value[:-2])


def main():
    values = {name: os.environ.get(name, "") for name in REQUIRED + OPTIONAL}
    missing = [name for name in REQUIRED if not values[name]]
    if missing:
        sys.exit("render-job.py: set " + ", ".join(missing))
    if values["MODE"] not in ("head", "base"):
        sys.exit(f"render-job.py: MODE is head or base, not {values['MODE']!r}")
    if values["TEN_TIMES"] not in ("0", "1"):
        sys.exit(f"render-job.py: TEN_TIMES is 0 or 1, not {values['TEN_TIMES']!r}")
    server, pg, pod = count("SERVER_CPU", values["SERVER_CPU"]), count("PG_CPU", values["PG_CPU"]), count("POD_CPU", values["POD_CPU"])
    if pod <= server + pg:
        sys.exit(f"render-job.py: POD_CPU {pod} leaves the driver no CPU beside the server's {server} and Postgres's {pg}")
    memory = gib("SERVER_MEMORY", values["SERVER_MEMORY"]) + gib("PG_MEMORY", values["PG_MEMORY"]) + gib("DRIVER_MEMORY", values["DRIVER_MEMORY"])
    gib("SCRATCH", values["SCRATCH"])
    if values["PAINT_RUNS"]:
        count("PAINT_RUNS", values["PAINT_RUNS"], "runs")
    values["DRIVER_CPU"] = str(pod - server - pg)
    values["CPU_HOURS"] = str(pod * ATTEMPT_HOURS)
    values["MEMORY_GIB_HOURS"] = str(memory * ATTEMPT_HOURS)
    names = "|".join(sorted(values, key=len, reverse=True))
    placeholder = re.compile(r"\$\{(" + names + r")\}")
    out = []
    for line in sys.stdin.read().splitlines(keepends=True):
        named = placeholder.findall(line)
        if any(name in OPTIONAL and not values[name] for name in named):
            continue
        out.append(placeholder.sub(lambda m: values[m[1]], line))
    sys.stdout.write("".join(out))


main()
