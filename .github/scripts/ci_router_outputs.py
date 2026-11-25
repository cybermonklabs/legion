#!/usr/bin/env python3
"""What each output of `.github/workflows/ci.yaml#changes` job evaluates to for one event, one
ref, one set of pull request labels, one set of workflow_dispatch inputs and one set of
paths-filter verdicts, read as GitHub Actions evaluates an expression. Its harness,
ci-router-outputs.test.sh, holds the outputs' label terms to what they promise: `full-ci` turns on
every slow suite, a `lane:<output>` label turns on that one output, and a label that names no
output turns on nothing (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request);
it holds a dispatched run's selection and the workflow's cancel key to what they promise: a
dispatch of main selects every job, a dispatch of a branch selects what a merge group with the
same verdicts selects and its filters are measured against its merge base with main as a merge
group's are against its base, and a newer dispatch of a branch cancels that branch's older one
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job);
and it reads, with --steps, whether each step a called workflow gates on what the changes job
hands it runs: the harness holds each test-job step gated on the one list of matched filters to
running exactly when its own filter matched
(.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates).

It reads the expressions themselves rather than a copy of them, so a change to an output's text
is what the harness tests. It runs the `changes` job's steps in order the way the runner would:
a step's `if:`, `env:` and `with:` are evaluated when it starts, over the outputs of the steps
before it alone, a step whose `if:` is false is skipped and its outputs read as null, a `run:`
step that carries an `id` runs in bash with its `env:` and GITHUB_EVENT_NAME, GITHUB_REF,
GITHUB_SHA and GITHUB_OUTPUT set, and its outputs are what it wrote there. Three steps it models
instead of running: the `filter` step's outputs are the verdicts given and the list below; the
`base` step's `sha` is the --base given, since that step fetches main's history to find it; and
the `generate` step writes nothing, as ci-filters-generate.sh writes to its outputs only when it
falls open (--step-outputs gives what it wrote then). One reader, runs(), decides whether each
job and step runs, here and under --steps. It evaluates the subset of GitHub's expression
language those use:
string, boolean, number and null literals; the contexts `github.event_name`, `github.ref`,
`github.sha`, `github.event.pull_request.number`, `github.event.pull_request.labels.*.name`,
`github.event.pull_request.base.sha`, `github.event.merge_group.base_sha`, `inputs.<name>` (a
workflow_dispatch input this workflow declares, and under --steps an input of the called
workflow), `steps.<id>.outputs.<name>` and `steps.<id>.outputs`, and under --steps
`needs.changes.result`, `needs.changes.outputs.<name>` and `needs.changes`; `!`, `==`, `!=`, `&&`,
`||` and parentheses; and the functions `contains` and `toJSON`. `&&` and `||` yield an operand,
as GitHub's do (a non-empty string such as 'false' is truthy), `==` and `!=` compare strings
without regard to case, `contains` of an array asks whether an element equals the item, and
`contains` of a string whether it holds the item, without regard to case. The paths-filter
step's `changes` output is the JSON array, without spaces as JSON.stringify writes it, of every
filter whose verdict is 'true', in the filters file's order, while no filter is named `changes`.
An output a step of the changes job did not write is empty, as GitHub reads one. Anything else, an
unknown function or a context it does not model, is refused by name with exit 2, never read as
false.

Usage: ci_router_outputs.py <workflow> --filters <file> --event <name> [--ref <ref>] [--sha <sha>]
  [--base <sha>] [--input <name>=<value>]... [--label <name>]... [--verdict <filter>=<value>]...
  [--verdicts <true|false>] [--step-outputs <id>=<file>]...
  [--filter-step | --generate-step | --concurrency | --steps <job> [--changes-result <result>]]
       ci_router_outputs.py <workflow> --label-readers
Prints `<output>=<value>` for each output of the `changes` job, in the file's order. --filters is
the changes job's filters, which it computes when it runs (.github/scripts/ci-filters-generate.sh)
and no file of the repository holds; the filter names are its keys. --verdicts sets every filter's
verdict (default true); --verdict overrides one. --ref defaults to a pull request's merge ref, the
queue's branch, or refs/heads/main for any other event; --sha to forty 1s. --base is the commit
the run is measured against, forty 3s by default: a pull request's base, a merge group's base, or
the merge base with main the `base` step finds on a dispatch. --step-outputs gives the outputs of
the changes job's step <id> as that step wrote them, a GITHUB_OUTPUT file of `<name>=<value>`
lines, in place of running or modelling it (the generate step's, for one: ci-filters-generate.sh
writes every_filter=true there when it falls open). --filter-step prints whether the filter step
runs and the `base` and `ref` it is handed when it starts; --generate-step prints the
CI_FILTERS_BASE the filter generation step is handed when it starts, its whole `env:` evaluated
there; --concurrency prints the workflow's concurrency `group` and
`cancel-in-progress`. --steps <job> prints instead, for the workflow that job of <workflow>
calls, `<step>=<true|false>` for each step whose `if:` reads an input the job fills from the
changes job, with that job's `with:` evaluated over those outputs and needs.changes.result
(default success). --label-readers prints, one per line, each output whose expression reads the
pull request's labels, found in the text rather than by evaluating it, so a broken term cannot
drop an output from that list.
Run from anywhere; ci.yaml's test job runs its harness."""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

TOKEN = re.compile(r"""\s*(?:
    (?P<string>'(?:[^']|'')*')
  | (?P<number>-?\d+(?:\.\d+)?)
  | (?P<op>==|!=|&&|\|\||!|\(|\)|,)
  | (?P<name>[A-Za-z_][A-Za-z0-9_-]*(?:\.(?:\*|[A-Za-z_][A-Za-z0-9_-]*))*)
)""", re.X)


class Refused(Exception):
    pass


# The contexts an expression may read, whole: the event's name, the run's ref and commit, the pull
# request's number, label names and base commit, the merge group's base commit, an input, and a
# step's outputs or one of them; and under --steps the changes job's result and outputs, which only
# that mode's context holds. A read of anything else, `github.event.pull_request` or `github.event`
# as a whole among them, is refused, so an output can read the labels only through the path
# --label-readers finds in its text.
MODELLED = re.compile(r"github\.event_name|github\.ref|github\.sha|github\.event\.pull_request\.number"
                      r"|github\.event\.pull_request\.labels\.\*\.name|github\.event\.pull_request\.base\.sha"
                      r"|github\.event\.merge_group\.base_sha|inputs\.[A-Za-z0-9_-]+"
                      r"|steps\.[A-Za-z0-9_-]+\.outputs(?:\.[A-Za-z0-9_-]+)?"
                      r"|needs\.changes(?:\.result|\.outputs(?:\.[A-Za-z0-9_-]+)?)?", re.I)
# An `inputs.<name>` read, and a value naming the changes job: what makes a step's `if:` one --steps
# reads, as .github/scripts/ci_filter_coverage.py resolves a called file's gates. GitHub reads
# context and property names in any case, so these do too: an `INPUTS.x` is the same read.
INPUT_REF = re.compile(r"\binputs\.([A-Za-z0-9_-]+)", re.I)
NAMES_CHANGES = re.compile(r"\bneeds\.changes\b", re.I)


class Skipped(dict):
    """The outputs of a step its `if:` skipped: any one of them reads as null, and the whole as
    an empty object, as GitHub reads them."""


def tokens(text):
    out, at = [], 0
    while at < len(text):
        if text[at:].strip() == "":
            break
        match = TOKEN.match(text, at)
        if not match or match.end() == at:
            raise Refused(f"cannot read {text[at:at + 20]!r}")
        kind = match.lastgroup
        out.append((kind, match.group(kind)))
        at = match.end()
    return out


def truthy(value):
    return value not in (False, None, "", 0) and value == value


def equal(a, b):
    if isinstance(a, str) and isinstance(b, str):
        return a.lower() == b.lower()
    return a == b


class Evaluator:
    def __init__(self, context):
        self.context = context

    def evaluate(self, text):
        self.items, self.at = tokens(text), 0
        value = self.either()
        if self.at != len(self.items):
            raise Refused(f"unread {self.items[self.at][1]!r} in {text!r}")
        return value

    def peek(self):
        return self.items[self.at][1] if self.at < len(self.items) else None

    def take(self, op=None):
        kind, value = self.items[self.at]
        if op is not None and value != op:
            raise Refused(f"expected {op!r}, read {value!r}")
        self.at += 1
        return kind, value

    def either(self):
        left = self.both()
        while self.peek() == "||":
            self.take()
            right = self.both()
            left = left if truthy(left) else right
        return left

    def both(self):
        left = self.compare()
        while self.peek() == "&&":
            self.take()
            right = self.compare()
            left = right if truthy(left) else left
        return left

    def compare(self):
        left = self.unary()
        while self.peek() in ("==", "!="):
            op = self.take()[1]
            right = self.unary()
            left = equal(left, right) if op == "==" else not equal(left, right)
        return left

    def unary(self):
        if self.peek() == "!":
            self.take()
            return not truthy(self.unary())
        return self.primary()

    def primary(self):
        kind, value = self.take()
        if value == "(":
            inner = self.either()
            self.take(")")
            return inner
        if kind == "string":
            return value[1:-1].replace("''", "'")
        if kind == "number":
            return float(value)
        if kind == "name":
            if value in ("true", "false"):
                return value == "true"
            if value == "null":
                return None
            if self.peek() == "(":
                return self.call(value)
            if not MODELLED.fullmatch(value):
                raise Refused(f"context {value} is outside the modelled contexts")
            return self.lookup(value)
        raise Refused(f"cannot read {value!r} here")

    def call(self, function):
        self.take("(")
        args = [self.either()]
        while self.peek() == ",":
            self.take()
            args.append(self.either())
        self.take(")")
        function = function.lower()
        if function == "contains" and len(args) == 2:
            search, item = args
            if isinstance(search, list):
                return any(equal(element, item) for element in search)
            return str(item).lower() in str(search).lower()
        if function == "tojson" and len(args) == 1:
            return json.dumps(args[0], indent=2)
        raise Refused(f"function {function} with {len(args)} arguments is not modelled")

    def lookup(self, path):
        parts = path.split(".")
        value = self.context
        for at, part in enumerate(parts):
            if part == "*":
                if not isinstance(value, list):
                    raise Refused(f"{'.'.join(parts[:at])} is no array in {path}")
                rest = ".".join(parts[at + 1:])
                return [Evaluator(element).lookup(rest) if rest else element for element in value]
            if isinstance(value, Skipped):
                return None
            # A property is found in any case, as GitHub's context dictionaries find it.
            key = next((k for k in value if str(k).lower() == part.lower()), None) if isinstance(value, dict) else None
            if key is None:
                # An output a step of the job never wrote reads as empty, as GitHub reads one.
                if at == 3 and len(parts) == 4 and parts[0].lower() == "steps" and parts[2].lower() == "outputs":
                    return ""
                raise Refused(f"context {path} is outside the modelled contexts")
            value = value[key]
        return value


def text_of(value):
    """A value as GitHub writes it into a string, an output or an input: a boolean as true or
    false, null as nothing."""
    if isinstance(value, bool):
        return "true" if value else "false"
    if value is None:
        return ""
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


EMBEDDED = re.compile(r"\$\{\{(.*?)\}\}", re.S)


def interpolate(text, evaluator):
    """A workflow value that may embed `${{ }}`: one expression and nothing else is that
    expression's value; text around expressions is a string with each written in by text_of."""
    text = str(text)
    whole = EMBEDDED.fullmatch(text.strip())
    if whole:
        return evaluator.evaluate(whole.group(1))
    return EMBEDDED.sub(lambda m: text_of(evaluator.evaluate(m.group(1))), text)


def expression_of(text):
    """The expression a `${{ }}` value holds, or None for a literal."""
    text = str(text).strip()
    return text[3:-2] if text.startswith("${{") and text.endswith("}}") else None


def runs(holder, evaluator, where):
    """Whether a job or a step runs: its `if:` as GitHub reads one, the whole text or the
    expression one `${{ }}` holds, and true when it has none. The one reader of a condition here,
    for the changes job's steps and for a called workflow's jobs and steps alike. An `if:` with no
    text, or one that is not a string or a boolean, is refused, naming where it is."""
    if "if" not in holder:
        return True
    value = holder["if"]
    if isinstance(value, bool):
        return value
    if not isinstance(value, str) or not value.strip():
        raise Refused(f"{where} has `if: {value!r}`, which holds no condition this reader can evaluate")
    text = value.strip()
    expression = expression_of(text)
    return truthy(evaluator.evaluate(expression if expression is not None else text))


def run_step(step, env, context):
    """A `run:` step's outputs: the script run by bash -e, as the runner's default shell runs it,
    with `env`, its `env:` as evaluated when it started, and the `name=value` lines it wrote to
    GITHUB_OUTPUT."""
    github = context["github"]
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "GITHUB_EVENT_NAME": github["event_name"],
           "GITHUB_REF": github["ref"], "GITHUB_SHA": github["sha"], **env}
    with tempfile.TemporaryDirectory() as scratch:
        env["GITHUB_OUTPUT"] = os.path.join(scratch, "output")
        open(env["GITHUB_OUTPUT"], "w").close()
        done = subprocess.run(["bash", "-e", "-c", str(step["run"])], env=env, capture_output=True, text=True)
        if done.returncode != 0:
            raise Refused(f"step {step['id']} exited {done.returncode}: {done.stderr.strip()}")
        outputs = {}
        for line in open(env["GITHUB_OUTPUT"]).read().splitlines():
            name, equals, value = line.partition("=")
            if not equals:
                raise Refused(f"step {step['id']} wrote {line!r} to GITHUB_OUTPUT, which is no name=value line")
            outputs[name] = value
    return outputs


def run_steps(job, context, filter_outputs, base, given):
    """Each step of the job in order, as the runner takes them, its outputs into
    context["steps"] under its `id`: a step given with --step-outputs has those outputs, the
    filter, base and generate steps are modelled as the docstring above says, and any other `run:`
    step runs. GitHub evaluates a step's `if:` and `env:` (and `with:`) when that step starts, over
    the outputs of the steps before it alone: a step that has not run yet reads as one that wrote
    nothing. So each step is read in the context as it stood when it started, and that context is
    returned for each step, in order, as (step, the context it started in, whether it ran), so a
    reader of a step's `env:` or `with:` reads them there and not after every step has run."""
    started = []
    for index, step in enumerate(job.get("steps") or []):
        at_start = dict(context, steps=dict(context["steps"]))
        evaluator = Evaluator(at_start)
        step_id = step.get("id")
        ran = runs(step, evaluator, f"step {step_id or index + 1} of the changes job")
        started.append((step, at_start, ran))
        if step_id is None:
            continue
        step_id = str(step_id)
        if not ran:
            outputs = Skipped()
        elif step_id in given:
            outputs = dict(given[step_id])
        elif step_id == "filter":
            outputs = dict(filter_outputs)
        elif step_id == "base":
            outputs = {"sha": base}
        elif step_id == "generate":
            outputs = {}
        elif "run" in step:
            env = {str(name): text_of(interpolate(value, evaluator)) for name, value in (step.get("env") or {}).items()}
            outputs = run_step(step, env, context)
        else:
            raise Refused(f"step {step_id} uses {step.get('uses')}, whose outputs this reader does not model")
        context["steps"][step_id] = {"outputs": outputs}
    return started


def started_step(started, chosen, what):
    """The one step of `started` that `chosen` picks, with the context it started in and whether it
    ran; refused, naming `what`, when the job holds none or more than one."""
    found = [entry for entry in started if chosen(entry[0])]
    if len(found) != 1:
        raise Refused(f"the changes job holds {len(found)} {what}, not one")
    return found[0]


def dispatch_inputs(document, event, given):
    """The `inputs` context: on a workflow_dispatch every input the workflow declares, at its
    default or the value given (a boolean input as a boolean), and on any other event every
    declared input as null. Naming an input the workflow does not declare is refused."""
    triggers = document.get("on", document.get(True)) or {}
    declared = ((triggers.get("workflow_dispatch") if isinstance(triggers, dict) else None) or {}).get("inputs") or {}
    inputs = {}
    for name, spec in declared.items():
        spec = spec or {}
        boolean = spec.get("type") == "boolean"
        default = spec.get("default", False if boolean else "")
        inputs[name] = default if event == "workflow_dispatch" else None
    for pair in given:
        name, _, value = pair.partition("=")
        if name not in declared:
            raise Refused(f"--input {pair} names no workflow_dispatch input this workflow declares")
        if event == "workflow_dispatch":
            inputs[name] = value == "true" if (declared[name] or {}).get("type") == "boolean" else value
    return inputs


def steps_of(workflow, document, job_id, outputs, context, result):
    """For the workflow job <job_id> of ci.yaml calls, each step whose `if:` reads an input the
    job fills from the changes job, with whether it runs: (step name, True|False), each condition
    read by runs(), as the changes job's steps are."""
    caller = (document.get("jobs") or {}).get(job_id)
    if not isinstance(caller, dict) or not str(caller.get("uses", "")).startswith("./"):
        raise Refused(f"job {job_id} of {workflow} calls no workflow of this repository")
    called_path = Path(workflow).resolve().parent.parent.parent / str(caller["uses"])[2:]
    called = yaml.safe_load(called_path.read_text())
    context = dict(context, needs={"changes": {"result": result, "outputs": outputs}})
    evaluator = Evaluator(context)
    passed, from_changes = {}, set()
    for name, value in (caller.get("with") or {}).items():
        expression = expression_of(value)
        passed[str(name)] = text_of(evaluator.evaluate(expression)) if expression is not None else text_of(value)
        if NAMES_CHANGES.search(str(value)):
            from_changes.add(str(name))
    evaluator = Evaluator(dict(context, inputs=passed))
    out = []
    for called_id, job in (called.get("jobs") or {}).items():
        job = job or {}
        # A file two jobs of ci.yaml call holds one job per caller, each `if: inputs.job == '<id>'`
        # (.github/ci-notes/ci.md#two-jobs-that-call-one-file): a job this caller does not run
        # runs none of its steps.
        if not runs(job, evaluator, f"job {called_id} of {called_path.name}"):
            continue
        for index, step in enumerate(job.get("steps") or []):
            step = step or {}
            condition = str(step.get("if", ""))
            if not {name.lower() for name in INPUT_REF.findall(condition)} & {name.lower() for name in from_changes}:
                continue
            name = str(step.get("name") or step.get("id") or step.get("run", "")).strip()
            out.append((name, runs(step, evaluator, f"step {index + 1} of job {called_id} of {called_path.name}")))
    return out


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("workflow")
    parser.add_argument("--event")
    parser.add_argument("--filters")
    parser.add_argument("--ref")
    parser.add_argument("--sha", default="1" * 40)
    parser.add_argument("--base", default="3" * 40)
    parser.add_argument("--input", action="append", default=[])
    parser.add_argument("--label-readers", action="store_true")
    parser.add_argument("--filter-step", action="store_true")
    parser.add_argument("--generate-step", action="store_true")
    parser.add_argument("--concurrency", action="store_true")
    parser.add_argument("--label", action="append", default=[])
    parser.add_argument("--verdict", action="append", default=[])
    parser.add_argument("--verdicts", default="true", choices=["true", "false"])
    parser.add_argument("--steps")
    parser.add_argument("--changes-result", default="success")
    parser.add_argument("--step-outputs", action="append", default=[])
    args = parser.parse_args()
    document = yaml.safe_load(Path(args.workflow).read_text())
    outputs = document["jobs"]["changes"]["outputs"]
    if args.label_readers:
        for name, expression in outputs.items():
            if "github.event.pull_request.labels" in str(expression):
                print(name)
        return 0
    if not args.event or not args.filters:
        parser.error("--event and --filters are required unless --label-readers is given")
    filters = yaml.safe_load(Path(args.filters).read_text())
    verdicts = {name: args.verdicts for name in filters}
    for pair in args.verdict:
        name, _, value = pair.partition("=")
        if name not in verdicts:
            print(f"ci_router_outputs: --verdict {pair} names no filter of {args.filters}", file=sys.stderr)
            return 2
        verdicts[name] = value
    # The action's own list of the filters that matched, which it writes only while no filter has
    # its name; JSON.stringify writes it without spaces.
    filter_outputs = dict(verdicts)
    if "changes" not in filters:
        filter_outputs["changes"] = json.dumps([name for name, value in verdicts.items() if value == "true"],
                                               separators=(",", ":"))
    changes = document["jobs"]["changes"]
    # Every step of the changes job that has an id, its outputs empty until a step writes one.
    ids = {str(step["id"]) for step in changes.get("steps") or [] if isinstance(step, dict) and step.get("id")}
    given = {}
    for pair in args.step_outputs:
        step_id, _, path = pair.partition("=")
        if step_id not in ids:
            print(f"ci_router_outputs: --step-outputs {pair} names no step of the changes job", file=sys.stderr)
            return 2
        given[step_id] = {}
        for line in Path(path).read_text().splitlines():
            name, equals, value = line.partition("=")
            if not equals or "<<" in name:
                print(f"ci_router_outputs: --step-outputs {pair}: {line!r} is no <name>=<value> line", file=sys.stderr)
                return 2
            given[step_id][name] = value
    ref = args.ref or {"pull_request": "refs/pull/1/merge",
                       "merge_group": f"refs/heads/gh-readonly-queue/main/pr-1-{args.sha}"}.get(args.event, "refs/heads/main")
    pull_request = args.event == "pull_request"
    try:
        context = {
            "github": {"event_name": args.event, "ref": ref, "sha": args.sha,
                       "event": {"pull_request": {"number": 1 if pull_request else None,
                                                  "labels": [{"name": label} for label in args.label],
                                                  "base": {"sha": args.base if pull_request else None}},
                                 "merge_group": {"base_sha": args.base if args.event == "merge_group" else None}}},
            "inputs": dispatch_inputs(document, args.event, args.input),
            "steps": {step_id: {"outputs": {}} for step_id in ids},
        }
        evaluator = Evaluator(context)
        if args.concurrency:
            concurrency = document.get("concurrency") or {}
            print(f"group={text_of(interpolate(concurrency.get('group', ''), evaluator))}")
            print(f"cancel-in-progress={text_of(truthy(interpolate(concurrency.get('cancel-in-progress', False), evaluator)))}")
            return 0
        started = run_steps(changes, context, filter_outputs, args.base, given)
        if args.filter_step:
            step, at_start, ran = started_step(started, lambda s: s.get("id") == "filter", "step of id filter")
            with_ = step.get("with") or {}
            print(f"runs={text_of(ran)}")
            for key in ("base", "ref"):
                print(f"{key}={text_of(interpolate(with_.get(key, ''), Evaluator(at_start)))}")
            return 0
        if args.generate_step:
            step, at_start, ran = started_step(started, lambda s: "ci-filters-generate.sh" in str(s.get("run", "")),
                                               "step that runs ci-filters-generate.sh")
            if not ran:
                raise Refused("the step that runs ci-filters-generate.sh is skipped, so it is handed no env")
            # Every value of the step's env, as the runner evaluates it when the step starts.
            env = {str(name): text_of(interpolate(value, Evaluator(at_start)))
                   for name, value in (step.get("env") or {}).items()}
            print(f"filters-base={env.get('CI_FILTERS_BASE', '')}")
            return 0
    except Refused as refusal:
        print(f"ci_router_outputs: {refusal}", file=sys.stderr)
        return 2
    values = {}
    for name, expression in outputs.items():
        text = expression_of(expression)
        if text is None:
            print(f"ci_router_outputs: output {name} is no expression: {str(expression).strip()}", file=sys.stderr)
            return 2
        try:
            values[name] = text_of(evaluator.evaluate(text))
        except Refused as refusal:
            print(f"ci_router_outputs: output {name}: {refusal}", file=sys.stderr)
            return 2
    if args.steps:
        try:
            for name, runs in steps_of(args.workflow, document, args.steps, values, context, args.changes_result):
                print(f"{name}={'true' if runs else 'false'}")
        except Refused as refusal:
            print(f"ci_router_outputs: --steps {args.steps}: {refusal}", file=sys.stderr)
            return 2
        return 0
    for name, value in values.items():
        print(f"{name}={value}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
