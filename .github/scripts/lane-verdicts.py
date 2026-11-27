#!/usr/bin/env python3
"""Say, in the changes job's log, what decided each slow suite on this run.

The paths filter's own log prints `Filter <name> = true` for every filter whose paths matched,
which for a slow suite on a pull request without its label is the opposite of what the run does:
the suite's output is false and the suite does not run on this head. This step prints, for each
output of `.github/workflows/ci.yaml#changes` that reads the pull request's labels, the verdict
that decided it: whether its paths matched, which label the pull request carries, whether the
suite runs on this run, and, for one it holds back, that the merge queue runs it. The decision is
the output's own expression, evaluated by ci_router_outputs.Evaluator over what this step is
handed, so the line cannot say what the output does not; ci-router-outputs.test.sh hands it the
env ci.yaml's step evaluates to and holds its decisions to the outputs.

A pull request that holds a suite back also gets one notice annotation naming every such suite,
and the job summary a table of them, so its reader sees them without opening this log
(.github/ci-notes/ci.md#the-changes-job-says-what-decided-each-slow-suite).

Its inputs, from the step's env: GITHUB_EVENT_NAME and GITHUB_STEP_SUMMARY, as Actions sets them;
EVERY_JOB, the scope step's every_job; LABELS, `toJSON(github.event.pull_request.labels.*.name)`;
FILTER and OWN, `toJSON(steps.filter.outputs)` and `toJSON(steps.own.outputs)`; SELECTED_BY,
ci.yaml's CI_OK_SELECTED_BY. With --decided it prints `<output>=<true|false>` for each such output
instead, which is what the harness compares.

Run from anywhere: .github/scripts/lane-verdicts.py [--decided]"""
import json
import os
import sys
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ci_router_outputs import Evaluator, Refused, expression_of, text_of  # noqa: E402

WORKFLOW = Path(__file__).resolve().parent.parent / "workflows" / "ci.yaml"


def read_json(name, default):
    text = os.environ.get(name, "")
    value = json.loads(text) if text.strip() else None
    return default if value is None else value


def main(argv):
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    labels = [str(label) for label in read_json("LABELS", [])]
    filter_outputs = read_json("FILTER", {})
    own_outputs = read_json("OWN", {})
    every_job = os.environ.get("EVERY_JOB", "")
    jobs_of = {}
    for job, output in read_json("SELECTED_BY", {}).items():
        jobs_of.setdefault(output, []).append(job)
    outputs = yaml.safe_load(WORKFLOW.read_text())["jobs"]["changes"]["outputs"]
    gated = [name for name, text in outputs.items() if "github.event.pull_request.labels" in str(text)]
    context = {
        "github": {"event_name": event,
                   "event": {"pull_request": {"labels": [{"name": label} for label in labels]}}},
        "steps": {"scope": {"outputs": {"every_job": every_job}},
                  "filter": {"outputs": filter_outputs},
                  "own": {"outputs": own_outputs}},
    }
    decided = {}
    for name in gated:
        try:
            decided[name] = text_of(Evaluator(context).evaluate(expression_of(outputs[name]))) == "true"
        except Refused as refusal:
            print(f"::error::lane-verdicts.py cannot read the output {name}: {refusal}")
            return 1
    if "--decided" in argv:
        for name in sorted(decided):
            print(f"{name}={'true' if decided[name] else 'false'}")
        return 0
    lowered = {label.lower() for label in labels}
    held, lines = [], []
    for name in gated:
        job = ", ".join(jobs_of.get(name, [name]))
        paths = filter_outputs.get(name) == "true"
        own = own_outputs.get(name) == "true"
        label = next((x for x in ("full-ci", f"lane:{name}") if x in lowered), "")
        if every_job:
            lines.append(f"{job}: runs, since this run selects every job")
        elif decided[name] and event == "pull_request":
            lines.append(f"{job}: runs on this head: its paths matched and the pull request carries {label}")
        elif decided[name]:
            why = "its paths matched" if paths else "the change touches one of its own packages"
            lines.append(f"{job}: runs on this {event} run: {why}")
        elif event != "pull_request" and (paths or own):
            # The output's expression turns these on on every run but a pull request.
            lines.append(f"::error::{job}: its paths matched or the change touches its own packages, and output "
                         f"{name} is still false on this {event} run")
        elif paths:
            held.append(job)
            lines.append(f"{job}: does not run on this head: its paths matched, the pull request carries neither "
                         f"full-ci nor lane:{name}, so changes holds it back; the merge queue runs it on the tree the "
                         f"change lands as")
        else:
            lines.append(f"{job}: not selected: its paths did not match")
    print("What decided each slow suite on this run (the paths filter's log above says only whether its paths "
          "matched):")
    for line in lines:
        print(line)
    if held:
        print(f"::notice title=Slow suites not run on this head::{', '.join(held)}: paths select them, the pull "
              f"request carries no label that runs them here, and the merge queue runs them")
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with open(summary, "a") as out:
                out.write("**Slow suites this pull request's paths select and this head does not run** "
                          "(the merge queue runs them; `full-ci` or `lane:<output>` runs one here):\n\n")
                for job in held:
                    out.write(f"- {job}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
