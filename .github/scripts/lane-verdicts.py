#!/usr/bin/env python3
"""Say, in the changes job's log, what decided each slow suite on this run.

The paths filter's own log prints `Filter <name> = true` for every filter whose paths matched,
which for a slow suite on a pull request without its label says the opposite of what the run does:
the suite's output is false and the suite does not run on this head. This step prints one line for
each output of `.github/workflows/ci.yaml#changes` that reads the pull request's labels
(ci_router_outputs.label_readers). Whether the line says the suite runs is the output's own
expression, evaluated by ci_router_outputs.Evaluator over what this step is handed; the reason
beside it is a fact the step is handed too: that the run selects every job, that the filter dorny
read was widened to every path, that the suite's paths matched, or the label the pull request
carries. A line that would contradict the output (paths matched and the label carried, and the
output false) is written as an `::error::` naming both, as is an output the evaluator cannot read.
Apart from those it writes no annotation: ci-ok's notice names the suites a run held back
(.github/ci-notes/ci.md#the-changes-job-says-what-decided-each-slow-suite).

Whether a filter was widened is read from the first line of the file the paths-filter step read,
which must be, as a whole line, one of the four ci-filters-generate.sh writes (FAIL_OPEN,
FORCED_ALL, FORCED_NAMES, and the computed filters' own header): the fail-open line and the
every-filter FORCED line (every filter), the FORCED line naming the filters that differ from the
base tree's (those), or the computed header (nothing widened). A file that cannot be read, or a
first line in no such form, is an `::error::` and exit 1, and each line that would say its paths
matched says instead that whether its filter was widened is UNKNOWN.

The computed header is the first line of ci_filter_coverage.GENERATED_HEAD, its owner's text, and
it is imported only when the first line is none of the other three forms, which need no import.
The fail-open file is what ci-filters-generate.sh writes when the filter engine fails, and on such
a run the engine may not import at all, while this script must still read the fail-open line. A
computed header is written only by an engine that ran, so its import is then safe; if it still
fails, that is the `::error::` above, never a header taken on trust.

ci-router-outputs.test.sh hands the script the env ci.yaml's step evaluates to, checks that its
FILTERS_FILE is the file the paths-filter step reads, feeds it files ci-filters-generate.sh itself
wrote, and holds each line to the output it is about.

Its inputs, from the step's env: GITHUB_EVENT_NAME, as Actions sets it; EVERY_JOB, the scope step's
every_job; FILTERS_FILE, the filters file the paths-filter step read; LABELS,
`toJSON(github.event.pull_request.labels.*.name)`; FILTER, `toJSON(steps.filter.outputs)`. The job
each output selects is read from ci.yaml's own CI_OK_SELECTED_BY.

Run from anywhere: .github/scripts/lane-verdicts.py"""
import json
import os
import re
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


# The first lines ci-filters-generate.sh writes into the file the paths-filter step reads, and the
# filters each widens to `'**'`, each matched as the whole line: the fail-open file (its `$why`
# free), the base it cannot read or compute (its reason free), and the filters whose patterns
# differ from the base tree's. The fourth, the computed filters copied as they are, is the first
# line of ci_filter_coverage.GENERATED_HEAD, read in widened() after these three (the module
# docstring says why).
FAIL_OPEN = re.compile(r"# FAIL-OPEN: the filters could not be computed \(.*\), so every filter matches every path "
                       r"and every job runs\.")
FORCED_ALL = re.compile(r"# FORCED: every filter matches every path: .+\.")
FORCED_NAMES = re.compile(r"# FORCED: ([\w-]+(?:, [\w-]+)*) differ from the base tree's filters, so each matches "
                          r"every path\.")


def widened(path):
    """(the filters the paths-filter step read as `'**'`, '*' for every one; or None, and why it is
    unknown), from the first line of the file at `path`."""
    if not path:
        return None, "FILTERS_FILE is not set, so which filters were widened to every path is unknown"
    try:
        # errors="replace": text-mode readline decodes a whole buffered chunk, so a byte that is
        # not UTF-8 on a later line must not refuse a good first line. One on the first line makes
        # it a line in no accepted form.
        with open(path, errors="replace") as fh:
            first = fh.readline().rstrip("\n")
    except OSError as error:
        return None, f"FILTERS_FILE {path} cannot be read ({error.strerror or error}), so which filters were widened to every path is unknown"
    if FAIL_OPEN.fullmatch(first) or FORCED_ALL.fullmatch(first):
        return {"*"}, None
    forced = FORCED_NAMES.fullmatch(first)
    if forced:
        return set(forced.group(1).split(", ")), None
    try:
        from ci_filter_coverage import GENERATED_HEAD
    except Exception as error:  # noqa: BLE001 - any failure to load the engine leaves the header unchecked
        return None, (f"the first line of FILTERS_FILE {path} is no fail-open or FORCED line, and the computed "
                      f"header cannot be checked: importing ci_filter_coverage failed ({type(error).__name__}: "
                      f"{error}), so which filters were widened to every path is unknown")
    if first == GENERATED_HEAD[0]:
        return set(), None
    return None, (f"the first line of FILTERS_FILE {path} is in none of the forms ci-filters-generate.sh writes "
                  f"({first[:160]!r}), so which filters were widened to every path is unknown")


MATCHED = {
    True: "its filter was widened to every path, since the filters could not be computed or the change may alter them",
    False: "its paths matched",
    None: "dorny matched its filter, and whether that filter was widened to every path is UNKNOWN (the error above says why)",
}


def line(job, name, runs, event, every_job, wide, paths, carried):
    """The line for one suite, and whether it is an error: what the output decided, with the fact
    that decided it."""
    matched = MATCHED[wide]
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


def main():
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    labels = [str(label) for label in read_json("LABELS", [])]
    filter_outputs = read_json("FILTER", {})
    every_job = os.environ.get("EVERY_JOB", "") == "true"
    wide, unknown = widened(os.environ.get("FILTERS_FILE", ""))
    if unknown:
        print(f"::error::lane-verdicts.py: {unknown}")
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
    lowered = {label.lower() for label in labels}
    print("What decided each slow suite on this run (the paths filter's log above says only whether its paths "
          "matched):")
    errors = 1 if unknown else 0
    for name, runs in decided.items():
        carried = [x for x in ("full-ci", f"lane:{name}") if x in lowered]
        text, error = line(", ".join(jobs_of.get(name, [name])), name, runs, event, every_job,
                           None if wide is None else ("*" in wide or name in wide),
                           filter_outputs.get(name) == "true", carried)
        print(text)
        errors += error
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
