#!/usr/bin/env python3
"""Fills job.yaml from the environment and prints the Job.

    python3 render-job.py < job.yaml > "$TMPDIR/job.yaml"

Each ${NAME} of job.yaml is the variable NAME. job.yaml's header names them and says what each
means. A variable it marks optional drops the line that names it when it is unset or empty; every
other variable is required, and the render refuses when one is missing. The render also computes
what follows from the mode and the sizes: the driver container's CPU (the pod's CPU less the
server's and Postgres's), one attempt's deadline (two hours, four in mode k14) and the Job's (three
attempts'), and the CPU-hours and memory GiB-hours of the Job's cost annotations at one attempt's
length. It names no compute class, runtime class, machine size, issue or session: the deployment
and the run that render the Job choose them.
"""
import os
import re
import sys

REQUIRED = (
    "COMMIT", "BASE", "MODE", "SCALE", "TEN_TIMES", "JOB_NAME", "OWNER", "BRANCH", "OMP_SESSION", "OMP_SESSION_TITLE",
    "LABEL_DOMAIN", "COMPUTE_CLASS", "SERVER_CPU", "SERVER_MEMORY", "PG_CPU", "PG_MEMORY", "POD_CPU", "DRIVER_MEMORY",
    "SCRATCH", "COST_USD",
)
OPTIONAL = ("RUNTIME_CLASS", "GATING_CLASS", "PAINT_RUNS", "CATCHUP")
ATTEMPT_HOURS = {"head": 2, "base": 2, "k14": 4}
ATTEMPTS = 3


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
    if values["MODE"] not in ATTEMPT_HOURS:
        sys.exit(f"render-job.py: MODE is head, base or k14, not {values['MODE']!r}")
    if values["TEN_TIMES"] not in ("0", "1"):
        sys.exit(f"render-job.py: TEN_TIMES is 0 or 1, not {values['TEN_TIMES']!r}")
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?", values["JOB_NAME"]):
        sys.exit(f"render-job.py: JOB_NAME is a Job name prefix of lower-case letters, digits and dashes, not {values['JOB_NAME']!r}")
    server, pg, pod = count("SERVER_CPU", values["SERVER_CPU"]), count("PG_CPU", values["PG_CPU"]), count("POD_CPU", values["POD_CPU"])
    if pod <= server + pg:
        sys.exit(f"render-job.py: POD_CPU {pod} leaves the driver no CPU beside the server's {server} and Postgres's {pg}")
    memory = gib("SERVER_MEMORY", values["SERVER_MEMORY"]) + gib("PG_MEMORY", values["PG_MEMORY"]) + gib("DRIVER_MEMORY", values["DRIVER_MEMORY"])
    gib("SCRATCH", values["SCRATCH"])
    if values["PAINT_RUNS"]:
        runs = count("PAINT_RUNS", values["PAINT_RUNS"], "runs")
        # k14 judges each project's two reads each as the first read after a restart once.
        if values["MODE"] == "k14" and runs < 2:
            sys.exit(f"render-job.py: PAINT_RUNS is at least 2 in mode k14, so each project's two reads are each the first after a restart once, not {runs}")
    if values["CATCHUP"] and not re.fullmatch(r"[1-9][0-9]*[hm]", values["CATCHUP"]):
        sys.exit(f"render-job.py: CATCHUP is a whole number of hours or minutes, such as 150m, not {values['CATCHUP']!r}")
    hours = ATTEMPT_HOURS[values["MODE"]]
    values["DRIVER_CPU"] = str(pod - server - pg)
    values["ATTEMPT_SECONDS"] = str(hours * 3600)
    values["JOB_DEADLINE_SECONDS"] = str(ATTEMPTS * hours * 3600)
    values["CPU_HOURS"] = str(pod * hours)
    values["MEMORY_GIB_HOURS"] = str(memory * hours)
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
