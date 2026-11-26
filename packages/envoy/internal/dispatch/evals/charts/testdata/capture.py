"""Capture the eval platform's answers to the chart corpus.

The specs file holds cases, each a list of chart declarations: `charts`, or `charts_json`, the same
list as JSON text, for a list that Python's json cannot write back as it was given (a float past a
float's range, such as 1e400, which Python reads as infinite, or a key given twice). A case may give
`previous`, the eval's newest stored charts, which the platform reads to carry a module forward.

This script runs each case through the platform's own validate_charts, imported from the checkout
that --platform names, and writes every case with the platform's answer: each message, in order,
with the field it names. It holds none of the platform's code, and the checkout stays outside the
repository. A case where Dispatch answers otherwise gives Dispatch's messages as `dispatch`, which
it writes the same way; charts_test.go holds Dispatch to that answer, and to the platform's
everywhere else.

    python3 testdata/capture.py --platform <checkout> --in testdata/corpus-specs.json --out testdata/corpus.json

--platform is the folder that holds the platform's `loaders` package, at the commit the specs file
names.
"""

import argparse
import json
import sys
from pathlib import Path


def field_of(message):
    """The field a message names: its text up to the first space outside a bracket, such as
    charts[0].states["a b"], without a possessive's apostrophe. charts.go's fieldOf reads it the
    same way."""
    depth, quoted, escaped = 0, False, False
    end = len(message)
    for index, char in enumerate(message):
        if quoted and escaped:
            escaped = False
        elif quoted and char == "\\":
            escaped = True
        elif quoted and char == '"':
            quoted = False
        elif quoted:
            pass
        elif char == '"' and depth > 0:
            quoted = True
        elif char == "[":
            depth += 1
        elif char == "]":
            depth -= 1
        elif char == " " and depth == 0:
            end = index
            break
    field = message[:end]
    return field[:-1] if field.endswith("'") else field


def corpus_text(commit, cases):
    """The corpus as JSON with one line for each case's declarations and one for each refusal, so a
    change to one case changes its own lines only."""
    lines = ['{"platform_commit": ' + json.dumps(commit) + ', "cases": [']
    for number, case in enumerate(cases):
        declared = {key: value for key, value in case.items() if key not in ("platform", "dispatch")}
        text = " " + json.dumps(declared, ensure_ascii=False)[:-1]
        for answer in ("platform", "dispatch"):
            if answer in case:
                text += f', "{answer}": [' + ",".join("\n  " + json.dumps(refusal, ensure_ascii=False)
                                                       for refusal in case[answer]) + "]"
        lines.append(text + "}" + ("," if number < len(cases) - 1 else ""))
    return "\n".join(lines + ["]}"]) + "\n"


def main():
    parser = argparse.ArgumentParser(description="Write each corpus case with the platform's answer.")
    parser.add_argument("--platform", required=True, help="the folder that holds the platform's loaders package")
    parser.add_argument("--in", dest="specs", required=True, help="the corpus specs, such as testdata/corpus-specs.json")
    parser.add_argument("--out", required=True, help="the corpus to write, such as testdata/corpus.json")
    args = parser.parse_args()

    sys.path.insert(0, str(Path(args.platform).resolve()))
    from loaders.register_rules import validate_charts

    specs = json.loads(Path(args.specs).read_text(encoding="utf-8"))
    cases = []
    for number, case in enumerate(specs["cases"]):
        name = case.get("name") or f"case {number}"
        if ("charts" in case) == ("charts_json" in case):
            sys.exit(f"{name}: give charts or charts_json, not both or neither")
        charts = json.loads(case["charts_json"]) if "charts_json" in case else case["charts"]
        try:
            messages = validate_charts(charts, case.get("previous"))
        except Exception as error:  # A case the platform cannot answer is no case of the corpus.
            sys.exit(f"{name}: the platform's validate_charts raised {type(error).__name__}: {error}")
        written = {key: case[key] for key in ("name", "kind", "difference", "charts", "charts_json", "previous")
                   if key in case}
        written["platform"] = [{"field": field_of(message), "message": message} for message in messages]
        if "dispatch" in case:
            written["dispatch"] = [{"field": field_of(message), "message": message} for message in case["dispatch"]]
        cases.append(written)

    Path(args.out).write_text(corpus_text(specs["platform_commit"], cases), encoding="utf-8")
    print(f"wrote {len(cases)} cases to {args.out}")


if __name__ == "__main__":
    main()
