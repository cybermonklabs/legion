#!/usr/bin/env python3
"""Say, in the changes job's log, what decided each slow suite on this run.

The paths filter's own log prints `Filter <name> = true` for every filter whose paths matched,
which for a slow suite on a pull request without its label says the opposite of what the run does:
the suite's output is false and the suite does not run on this head. This step prints, for each
output of `.github/workflows/ci.yaml#changes` that reads the pull request's labels
(ci_router_outputs.label_readers), the verdict that decided it: whether its paths matched, which
label the pull request carries, whether the suite runs on this run, and, for one it holds back,
that the merge queue runs it. The decision is the output's own expression, evaluated by
ci_router_outputs.Evaluator over what this step is handed, so the line cannot say what the output
does not; ci-router-outputs.test.sh hands it the env ci.yaml's step evaluates to and holds its
decisions to the outputs. It writes no annotation: ci-ok's notice names the suites a run held back
(.github/ci-notes/ci.md#the-changes-job-says-what-decided-each-slow-suite).

Its inputs, from the step's env: GITHUB_EVENT_NAME, as Actions sets it; EVERY_JOB, the scope step's
every_job; LABELS, `toJSON(github.event.pull_request.labels.*.name)`; FILTER,
`toJSON(steps.filter.outputs)`; SELECTED_BY, ci.yaml's CI_OK_SELECTED_BY. With --decided it prints
`<output>=<true|false>` for each such output instead, which is what the harness compares.

Run from anywhere: .github/scripts/lane-verdicts.py [--decided]"""
import json
import os
import sys
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
from ci_router_outputs import Evaluator, Refused, expression_of, label_readers, text_of  # noqa: E402

WORKFLOW = Path(__file__).resolve().parent.parent / "workflows" / "ci.yaml"


def read_json(name, default):
    text = os.environ.get(name, "")
    value = json.loads(text) if text.strip() else None
    return default if value is None else value


def main(argv):
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    labels = [str(label) for label in read_json("LABELS", [])]
    filter_outputs = read_json("FILTER", {})
    every_job = os.environ.get("EVERY_JOB", "")
    jobs_of = {}
    for job, output in read_json("SELECTED_BY", {}).items():
        jobs_of.setdefault(output, []).append(job)
    outputs = yaml.safe_load(WORKFLOW.read_text())["jobs"]["changes"]["outputs"]
    context = {
        "github": {"event_name": event,
                   "event": {"pull_request": {"labels": [{"name": label} for label in labels]}}},
        "steps": {"scope": {"outputs": {"every_job": every_job}},
                  "filter": {"outputs": filter_outputs}},
    }
    decided = {}
    for name in label_readers(outputs):
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
    print("What decided each slow suite on this run (the paths filter's log above says only whether its paths "
          "matched):")
    for name, runs in decided.items():
        job = ", ".join(jobs_of.get(name, [name]))
        paths = filter_outputs.get(name) == "true"
        label = next((x for x in ("full-ci", f"lane:{name}") if x in lowered), "")
        if every_job:
            print(f"{job}: runs, since this run selects every job")
        elif runs and event == "pull_request":
            print(f"{job}: runs on this head: its paths matched and the pull request carries {label}")
        elif runs:
            print(f"{job}: runs on this {event} run: its paths matched")
        elif paths:
            print(f"{job}: does not run on this head: its paths matched, the pull request carries neither "
                  f"full-ci nor lane:{name}, so changes holds it back; the merge queue runs it on the tree the "
                  f"change lands as")
        else:
            print(f"{job}: not selected: its paths did not match")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
