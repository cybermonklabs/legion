#!/usr/bin/env python3
"""What each output of ci.yaml's `changes` job evaluates to for one event, one set of pull request
labels and one set of paths-filter verdicts, read as GitHub Actions evaluates an expression. Its
harness, ci-router-outputs.test.sh, holds the outputs' label terms to what they promise: `full-ci`
turns on every slow suite, a `lane:<output>` label turns on that one output, and a label that names
no output turns on nothing (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request).
It also reads, with --steps, whether each step a called workflow gates on what the changes job
hands it runs: the harness holds each test-job step gated on the one list of matched filters to
running exactly when its own filter matched
(.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates).

It reads the expressions themselves rather than a copy of them, so a change to an output's text
is what the harness tests. It evaluates the subset of GitHub's expression language those outputs
use: string, boolean, number and null literals; the contexts `github.event_name`,
`github.event.pull_request.labels.*.name`, `steps.<id>.outputs.<name>` and `steps.<id>.outputs`,
and under --steps `needs.changes.result`, `needs.changes.outputs.<name>`, `needs.changes` and
`inputs.<name>`; `!`, `==`, `!=`, `&&`, `||` and parentheses; and the functions `contains` and
`toJSON`. `&&` and `||` yield an operand, as GitHub's do (a non-empty string such as 'false' is
truthy), `==` and `!=` compare strings without regard to case, `contains` of an array asks
whether an element equals the item, and `contains` of a string whether it holds the item, without
regard to case. The paths-filter step's `changes` output is the JSON array, without spaces as
JSON.stringify writes it, of every filter whose verdict is 'true', in the filters file's order,
while no filter is named `changes`. An output a step of the changes job did not write is empty,
as GitHub reads one. Anything else, an unknown function or a context it does not model, is
refused by name with exit 2, never read as false.

Usage: ci_router_outputs.py <workflow> --filters <file> --event <name> [--label <name>]...
  [--verdict <filter>=<value>]... [--verdicts <true|false>] [--step-outputs <id>=<file>]...
  [--steps <job> [--changes-result <result>]]
       ci_router_outputs.py <workflow> --label-readers
Prints `<output>=<value>` for each output of the `changes` job, in the file's order. --filters is
the changes job's filters, which it computes when it runs (.github/scripts/ci-filters-generate.sh)
and no file of the repository holds; the filter names are its keys. --verdicts sets every filter's
verdict (default true); --verdict overrides one. --step-outputs gives the outputs of the changes
job's step <id> as that step wrote them, a GITHUB_OUTPUT file of `<name>=<value>` lines (the
generate step's, for one: ci-filters-generate.sh writes every_filter=true there when it falls
open). --steps <job> prints instead, for the workflow
that job of <workflow> calls, `<step>=<true|false>` for each step whose `if:` reads an input the
job fills from the changes job, with that job's `with:` evaluated over those outputs and
needs.changes.result (default success). --label-readers prints, one per line, each output whose
expression reads the pull request's labels, found in the text rather than by evaluating it, so a
broken term cannot drop an output from that list.
Run from anywhere; ci.yaml's test job runs its harness."""
import argparse
import json
import re
import sys
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


# The contexts an expression may read, whole: the event's name, the pull request's label names,
# and a step's outputs or one of them; and under --steps the changes job's result and outputs and
# an input of the called workflow, which only that mode's context holds. A read of anything else,
# `github.event.pull_request` or `github.event` as a whole among them, is refused, so an output can
# read the labels only through the path --label-readers finds in its text.
MODELLED = re.compile(r"github\.event_name|github\.event\.pull_request\.labels\.\*\.name"
                      r"|steps\.[A-Za-z0-9_-]+\.outputs(?:\.[A-Za-z0-9_-]+)?"
                      r"|needs\.changes(?:\.result|\.outputs(?:\.[A-Za-z0-9_-]+)?)?|inputs\.[A-Za-z0-9_-]+", re.I)
# An `inputs.<name>` read, and a value naming the changes job: what makes a step's `if:` one --steps
# reads, as .github/scripts/ci_filter_coverage.py resolves a called file's gates. GitHub reads
# context and property names in any case, so these do too: an `INPUTS.x` is the same read.
INPUT_REF = re.compile(r"\binputs\.([A-Za-z0-9_-]+)", re.I)
NAMES_CHANGES = re.compile(r"\bneeds\.changes\b", re.I)


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
            # A property is found in any case, as GitHub's context dictionaries find it.
            key = next((k for k in value if str(k).lower() == part.lower()), None) if isinstance(value, dict) else None
            if key is None:
                # An output a step of the job never wrote reads as empty, as GitHub reads one.
                if at == 3 and len(parts) == 4 and parts[0].lower() == "steps" and parts[2].lower() == "outputs":
                    return ""
                raise Refused(f"context {path} is outside the modelled contexts")
            value = value[key]
        return value


def as_text(value):
    """An expression's value as the string GitHub hands on in an output or an input."""
    if isinstance(value, bool):
        return "true" if value else "false"
    return "" if value is None else str(value)


def expression_of(text):
    """The expression a `${{ }}` value holds, or None for a literal."""
    text = str(text).strip()
    return text[3:-2] if text.startswith("${{") and text.endswith("}}") else None


def steps_of(workflow, document, job_id, outputs, context, result):
    """For the workflow job <job_id> of ci.yaml calls, each step whose `if:` reads an input the
    job fills from the changes job, with whether it runs: (step name, True|False). A step whose
    `if:` has no `${{ }}` is read whole, as GitHub reads one."""
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
        passed[str(name)] = as_text(evaluator.evaluate(expression)) if expression is not None else as_text(value)
        if NAMES_CHANGES.search(str(value)):
            from_changes.add(str(name))
    evaluator = Evaluator(dict(context, inputs=passed))
    out = []
    for job in (called.get("jobs") or {}).values():
        # A file two jobs of ci.yaml call holds one job per caller, each `if: inputs.job == '<id>'`
        # (.github/ci-notes/ci.md#two-jobs-that-call-one-file): a job this caller does not run
        # runs none of its steps.
        job_condition = str((job or {}).get("if", "")).strip()
        if job_condition:
            expression = expression_of(job_condition)
            if not truthy(evaluator.evaluate(expression if expression is not None else job_condition)):
                continue
        for step in (job or {}).get("steps") or []:
            condition = str((step or {}).get("if", ""))
            if not {name.lower() for name in INPUT_REF.findall(condition)} & {name.lower() for name in from_changes}:
                continue
            expression = expression_of(condition)
            name = str(step.get("name") or step.get("id") or step.get("run", "")).strip()
            out.append((name, truthy(evaluator.evaluate(expression if expression is not None else condition))))
    return out


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("workflow")
    parser.add_argument("--event")
    parser.add_argument("--filters")
    parser.add_argument("--label-readers", action="store_true")
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
    step_outputs = dict(verdicts)
    if "changes" not in filters:
        step_outputs["changes"] = json.dumps([name for name, value in verdicts.items() if value == "true"],
                                             separators=(",", ":"))
    context = {
        "github": {"event_name": args.event,
                   "event": {"pull_request": {"labels": [{"name": label} for label in args.label]}}},
        # Every step of the changes job that has an id, its outputs empty until a step writes one.
        "steps": {str(step["id"]): {"outputs": {}} for step in document["jobs"]["changes"].get("steps") or []
                  if isinstance(step, dict) and step.get("id")},
    }
    context["steps"]["filter"] = {"outputs": step_outputs}
    context["steps"]["ref"] = {"outputs": {"is_main": "false"}}
    for pair in args.step_outputs:
        step_id, _, path = pair.partition("=")
        if step_id not in context["steps"]:
            print(f"ci_router_outputs: --step-outputs {pair} names no step of the changes job", file=sys.stderr)
            return 2
        for line in Path(path).read_text().splitlines():
            name, equals, value = line.partition("=")
            if not equals or "<<" in name:
                print(f"ci_router_outputs: --step-outputs {pair}: {line!r} is no <name>=<value> line", file=sys.stderr)
                return 2
            context["steps"][step_id]["outputs"][name] = value
    evaluator = Evaluator(context)
    values = {}
    for name, expression in outputs.items():
        text = expression_of(expression)
        if text is None:
            print(f"ci_router_outputs: output {name} is no expression: {str(expression).strip()}", file=sys.stderr)
            return 2
        try:
            values[name] = as_text(evaluator.evaluate(text))
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
