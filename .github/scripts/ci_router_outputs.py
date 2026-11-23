#!/usr/bin/env python3
"""What each output of ci.yaml's `changes` job evaluates to for one event, one ref, one set of
pull request labels, one set of workflow_dispatch inputs and one set of paths-filter verdicts,
read as GitHub Actions evaluates an expression. Its harness, ci-router-outputs.test.sh, holds the
outputs' label terms to what they promise: `full-ci` turns on every slow suite, a `lane:<output>`
label turns on that one output, and a label that names no output turns on nothing
(.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request); and it holds a
dispatched run's selection and the workflow's cancel key to what they promise: a dispatch of main
selects every job, a dispatch of a branch selects what a merge group with the same verdicts
selects, and a newer dispatch of a branch cancels that branch's older one
(.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job).

It reads the expressions themselves rather than a copy of them, so a change to an output's text
is what the harness tests. It runs the `changes` job's steps that carry an `id` the way the runner
would: a step whose `if:` is false is skipped and its outputs read as null; a `run:` step runs in
bash with its `env:` evaluated and GITHUB_EVENT_NAME, GITHUB_REF, GITHUB_SHA and GITHUB_OUTPUT
set, and its outputs are what it wrote there; the `filter` step's outputs are the verdicts given.
It evaluates the subset of GitHub's expression language those use: string, boolean, number and
null literals; the contexts `github.event_name`, `github.ref`, `github.sha`,
`github.event.pull_request.number`, `github.event.pull_request.labels.*.name`,
`inputs.<name>` (a workflow_dispatch input this workflow declares), `steps.<id>.outputs.<name>`
and `steps.<id>.outputs`; `!`, `==`, `!=`, `&&`, `||` and parentheses; and the functions
`contains` and `toJSON`. `&&` and `||` yield an operand, as GitHub's do (a non-empty string such
as 'false' is truthy), `==` and `!=` compare strings without regard to case, and `contains` of an
array asks whether an element equals the item. Anything else, an unknown function or a context it
does not model, is refused by name with exit 2, never read as false.

Usage: ci_router_outputs.py <workflow> --event <name> [--ref <ref>] [--sha <sha>] [--input <name>=<value>]...
  [--label <name>]... [--verdict <filter>=<value>]... [--verdicts <true|false>] [--filter-step | --concurrency]
       ci_router_outputs.py <workflow> --label-readers
Prints `<output>=<value>` for each output of the `changes` job, in the file's order. --verdicts
sets every filter's verdict (default true); --verdict overrides one. --ref defaults to a pull
request's merge ref, the queue's branch, or refs/heads/main for any other event; --sha to forty
1s. --filter-step prints whether the filter step runs and the `base` and `ref` it is handed;
--concurrency prints the workflow's concurrency `group` and `cancel-in-progress`. --label-readers
prints, one per line, each output whose expression reads the pull request's labels, found in the
text rather than by evaluating it, so a broken term cannot drop an output from that list.
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
# request's number and label names, a workflow_dispatch input, and a step's outputs or one of
# them. A read of anything else, `github.event.pull_request` or `github.event` as a whole among
# them, is refused, so an output can read the labels only through the path --label-readers finds
# in its text.
MODELLED = re.compile(r"github\.event_name|github\.ref|github\.sha|github\.event\.pull_request\.number"
                      r"|github\.event\.pull_request\.labels\.\*\.name|inputs\.[A-Za-z0-9_-]+"
                      r"|steps\.[A-Za-z0-9_-]+\.outputs(?:\.[A-Za-z0-9_-]+)?")


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
        if function == "contains" and len(args) == 2:
            search, item = args
            if isinstance(search, list):
                return any(equal(element, item) for element in search)
            return str(item).lower() in str(search).lower()
        if function == "toJSON" and len(args) == 1:
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
            if not isinstance(value, dict) or part not in value:
                raise Refused(f"context {path} is outside the modelled contexts")
            value = value[part]
        return value


def text_of(value):
    """A value as GitHub writes it into a string: a boolean as true or false, null as nothing."""
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


def condition(step, evaluator):
    """Whether a step runs: its `if:`, with or without `${{ }}`, and true when it has none."""
    if "if" not in step:
        return True
    text = str(step["if"]).strip()
    whole = EMBEDDED.fullmatch(text)
    return truthy(evaluator.evaluate(whole.group(1) if whole else text))


def run_step(step, context, evaluator):
    """A `run:` step's outputs: the script run by bash -e, as the runner's default shell runs it,
    with its `env:` evaluated, and the `name=value` lines it wrote to GITHUB_OUTPUT."""
    github = context["github"]
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "GITHUB_EVENT_NAME": github["event_name"],
           "GITHUB_REF": github["ref"], "GITHUB_SHA": github["sha"]}
    for name, value in (step.get("env") or {}).items():
        env[str(name)] = text_of(interpolate(value, evaluator))
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


def run_steps(job, context, evaluator, verdicts):
    """Each step of the job that carries an `id`, in order, into context["steps"]."""
    for step in job.get("steps") or []:
        step_id = step.get("id")
        if step_id is None:
            continue
        if not condition(step, evaluator):
            outputs = Skipped()
        elif "run" in step:
            outputs = run_step(step, context, evaluator)
        elif step_id == "filter":
            outputs = dict(verdicts)
        else:
            raise Refused(f"step {step_id} uses {step.get('uses')}, whose outputs this reader does not model")
        context["steps"][step_id] = {"outputs": outputs}


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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("workflow")
    parser.add_argument("--event")
    parser.add_argument("--ref")
    parser.add_argument("--sha", default="1" * 40)
    parser.add_argument("--input", action="append", default=[])
    parser.add_argument("--label-readers", action="store_true")
    parser.add_argument("--filter-step", action="store_true")
    parser.add_argument("--concurrency", action="store_true")
    parser.add_argument("--label", action="append", default=[])
    parser.add_argument("--verdict", action="append", default=[])
    parser.add_argument("--verdicts", default="true", choices=["true", "false"])
    args = parser.parse_args()
    document = yaml.safe_load(Path(args.workflow).read_text())
    outputs = document["jobs"]["changes"]["outputs"]
    if args.label_readers:
        for name, expression in outputs.items():
            if "github.event.pull_request.labels" in str(expression):
                print(name)
        return 0
    if not args.event:
        parser.error("--event is required unless --label-readers is given")
    filters = yaml.safe_load(Path(args.workflow).parent.parent.joinpath("ci-filters.yaml").read_text())
    verdicts = {name: args.verdicts for name in filters}
    for pair in args.verdict:
        name, _, value = pair.partition("=")
        if name not in verdicts:
            print(f"ci_router_outputs: --verdict {pair} names no filter of ci-filters.yaml", file=sys.stderr)
            return 2
        verdicts[name] = value
    ref = args.ref or {"pull_request": "refs/pull/1/merge",
                       "merge_group": f"refs/heads/gh-readonly-queue/main/pr-1-{args.sha}"}.get(args.event, "refs/heads/main")
    pull_request = args.event == "pull_request"
    try:
        context = {
            "github": {"event_name": args.event, "ref": ref, "sha": args.sha,
                       "event": {"pull_request": {"number": 1 if pull_request else None,
                                                  "labels": [{"name": label} for label in args.label]}}},
            "inputs": dispatch_inputs(document, args.event, args.input),
            "steps": {},
        }
        evaluator = Evaluator(context)
        if args.concurrency:
            concurrency = document.get("concurrency") or {}
            print(f"group={text_of(interpolate(concurrency.get('group', ''), evaluator))}")
            print(f"cancel-in-progress={text_of(truthy(interpolate(concurrency.get('cancel-in-progress', False), evaluator)))}")
            return 0
        changes = document["jobs"]["changes"]
        run_steps(changes, context, evaluator, verdicts)
        if args.filter_step:
            step = next(s for s in changes.get("steps") or [] if s.get("id") == "filter")
            given = step.get("with") or {}
            print(f"runs={text_of(not isinstance(context['steps']['filter']['outputs'], Skipped))}")
            for key in ("base", "ref"):
                print(f"{key}={text_of(interpolate(given.get(key, ''), evaluator))}")
            return 0
    except Refused as refusal:
        print(f"ci_router_outputs: {refusal}", file=sys.stderr)
        return 2
    for name, expression in outputs.items():
        text = str(expression).strip()
        if not (text.startswith("${{") and text.endswith("}}")):
            print(f"ci_router_outputs: output {name} is no expression: {text}", file=sys.stderr)
            return 2
        try:
            value = evaluator.evaluate(text[3:-2])
        except Refused as refusal:
            print(f"ci_router_outputs: output {name}: {refusal}", file=sys.stderr)
            return 2
        if isinstance(value, bool):
            value = "true" if value else "false"
        print(f"{name}={value}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
