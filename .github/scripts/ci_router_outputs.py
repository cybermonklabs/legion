#!/usr/bin/env python3
"""What each output of ci.yaml's `changes` job evaluates to for one event, one set of pull request
labels and one set of paths-filter verdicts, read as GitHub Actions evaluates an expression. Its
harness, ci-router-outputs.test.sh, holds the outputs' label terms to what they promise: `full-ci`
turns on every slow suite, a `lane:<output>` label turns on that one output, and a label that names
no output turns on nothing (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request).

It reads the expressions themselves rather than a copy of them, so a change to an output's text
is what the harness tests. It evaluates the subset of GitHub's expression language those outputs
use: string, boolean, number and null literals; the contexts `github.event_name`,
`github.event.pull_request.labels.*.name`, `steps.<id>.outputs.<name>` and `steps.<id>.outputs`;
`!`, `==`, `!=`, `&&`, `||` and parentheses; and the functions `contains` and `toJSON`. `&&` and
`||` yield an operand, as GitHub's do (a non-empty string such as 'false' is truthy), `==` and
`!=` compare strings without regard to case, and `contains` of an array asks whether an element
equals the item. Anything else, an unknown function or a context it does not model, is refused by
name with exit 2, never read as false.

Usage: ci_router_outputs.py <workflow> --event <name> [--label <name>]... [--verdict <filter>=<value>]...
  [--verdicts <true|false>]
       ci_router_outputs.py <workflow> --label-readers
Prints `<output>=<value>` for each output of the `changes` job, in the file's order. --verdicts
sets every filter's verdict (default true); --verdict overrides one. --label-readers prints, one
per line, each output whose expression reads the pull request's labels, found in the text
rather than by evaluating it, so a broken term cannot drop an output from that list.
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
# and a step's outputs or one of them. A read of anything else, `github.event.pull_request` or
# `github.event` as a whole among them, is refused, so an output can read the labels only
# through the path --label-readers finds in its text.
MODELLED = re.compile(r"github\.event_name|github\.event\.pull_request\.labels\.\*\.name"
                      r"|steps\.[A-Za-z0-9_-]+\.outputs(?:\.[A-Za-z0-9_-]+)?")


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
            if not isinstance(value, dict) or part not in value:
                raise Refused(f"context {path} is outside the modelled contexts")
            value = value[part]
        return value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("workflow")
    parser.add_argument("--event")
    parser.add_argument("--label-readers", action="store_true")
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
    context = {
        "github": {"event_name": args.event,
                   "event": {"pull_request": {"labels": [{"name": label} for label in args.label]}}},
        "steps": {"filter": {"outputs": verdicts}, "ref": {"outputs": {"is_main": "false"}}},
    }
    evaluator = Evaluator(context)
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
