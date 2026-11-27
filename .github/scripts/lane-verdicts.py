#!/usr/bin/env python3
"""Say, in the changes job's log, what decided each slow suite on this run.

The paths filter's own log prints `Filter <name> = true` for every filter whose paths matched,
which for a slow suite on a pull request without its label says the opposite of what the run does:
the suite's output is false and the suite does not run on this head. This step prints one line for
each output of `.github/workflows/ci.yaml#changes` that reads the pull request's labels
(ci_router_outputs.label_readers). Whether the line says the suite runs is the output's own
expression, evaluated by ci_router_outputs.Evaluator over what this step is handed; the reason
beside it is a fact the step is handed too: that the run selects every job, that the filter dorny
read was widened to every path (the filters file's first line, which ci-filters-generate.sh writes
`# FAIL-OPEN:` or `# FORCED:` when it falls open, or when the change may alter the computation),
that the suite's paths matched, or the label the pull request carries. A line that would
contradict the output (paths matched and the label carried, and the output false) is written as an
`::error::` naming both, as is an output the evaluator cannot read. Apart from those it writes no
annotation: ci-ok's notice names the suites a run held back
(.github/ci-notes/ci.md#the-changes-job-says-what-decided-each-slow-suite).
ci-router-outputs.test.sh hands the script the env ci.yaml's step evaluates to and holds each line
to the output it is about.

Its inputs, from the step's env: GITHUB_EVENT_NAME, as Actions sets it; EVERY_JOB, the scope step's
every_job; FILTERS_FILE, the filters file the paths-filter step read; LABELS, `toJSON(github.event.pull_request.labels.*.name)`; FILTER, `toJSON(steps.filter.outputs)`.
The job each output selects is read from ci.yaml's own CI_OK_SELECTED_BY. With --decided it prints
`<output>=<true|false>` for each such output instead, which the harness compares with the outputs.

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


def widened(path):
    """The filters the paths-filter step read as `'**'`, from its file's first line: every one
    (`# FAIL-OPEN:`, or `# FORCED: every filter matches every path`), those a `# FORCED:` line names
    as differing from the base tree's, or none."""
    try:
        first = open(path).readline().strip()
    except OSError:
        return set()
    if first.startswith("# FAIL-OPEN:") or first.startswith("# FORCED: every filter matches every path"):
        return {"*"}
    if first.startswith("# FORCED: ") and " differ from the base tree" in first:
        return {name.strip() for name in first[len("# FORCED: "):].split(" differ from")[0].split(",")}
    return set()


def line(job, name, runs, event, every_job, wide, paths, carried):
    """The line for one suite, and whether it is an error: what the output decided, with the fact
    that decided it."""
    matched = ("its filter was widened to every path, since the filters could not be computed or the change may "
               "alter them" if wide else "its paths matched")
    if runs:
        if every_job:
            return f"{job}: runs, since this run selects every job", False
        if not paths:
            return f"::error::{job}: output {name} is true and its filter did not match", True
        if event != "pull_request":
            return f"{job}: runs on this {event} run: {matched}", False
        if not carried:
            return f"::error::{job}: output {name} is true on this pull request, which carries no label that runs it", True
        return f"{job}: runs on this head: {matched} and the pull request carries {' and '.join(carried)}", False
    if every_job:
        return f"::error::{job}: this run selects every job and output {name} is false", True
    if not paths:
        return f"{job}: not selected: its paths did not match", False
    if event != "pull_request" or carried:
        held = f" and the pull request carries {' and '.join(carried)}" if carried else ""
        return f"::error::{job}: {matched}{held}, and output {name} is false on this {event} run", True
    return (f"{job}: does not run on this head: {matched} and the pull request does not carry lane:{name}, so "
            f"changes holds it back; the merge queue runs the suites its own generator selects against its own "
            f"base, which may be fewer than this head's selection"), False


def main(argv):
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    labels = [str(label) for label in read_json("LABELS", [])]
    filter_outputs = read_json("FILTER", {})
    every_job = os.environ.get("EVERY_JOB", "") == "true"
    wide = widened(os.environ.get("FILTERS_FILE", ""))
    document = yaml.safe_load(WORKFLOW.read_text())
    jobs_of = {}
    for job, output in json.loads(document["env"]["CI_OK_SELECTED_BY"]).items():
        jobs_of.setdefault(output, []).append(job)
    outputs = document["jobs"]["changes"]["outputs"]
    context = {
        "github": {"event_name": event,
                   "event": {"pull_request": {"labels": [{"name": label} for label in labels]}}},
        "steps": {"scope": {"outputs": {"every_job": "true" if every_job else ""}},
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
    errors = 0
    for name, runs in decided.items():
        carried = [x for x in ("full-ci", f"lane:{name}") if x in lowered]
        text, error = line(", ".join(jobs_of.get(name, [name])), name, runs, event, every_job,
                           "*" in wide or name in wide, filter_outputs.get(name) == "true", carried)
        print(text)
        errors += error
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
