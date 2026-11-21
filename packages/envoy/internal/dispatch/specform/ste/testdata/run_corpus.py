"""Print ste-lint's hard findings for each line of a corpus file.

Usage: python3 run_corpus.py [corpus.jsonl]

Each line of the corpus is one JSON string. For each line, in order, this prints one JSON line:
the list of {"rule", "match"} of the hard ("advisory-free") findings that ste-lint.py, beside
this file, gives that string, in ste-lint's order. TestPortMatchesSteLintOnTheCorpus compares
them with the Go port's.
"""
import importlib.util
import json
import pathlib
import sys

here = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("ste_lint", here / "ste-lint.py")
ste = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ste)

corpus = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else here / "corpus.jsonl"
with open(corpus, encoding="utf-8", newline="") as source:
    lines = source.read().split("\n")
if lines and lines[-1] == "":
    lines.pop()
for line in lines:
    findings, _ = ste.lint(json.loads(line))
    hard = [{"rule": f["rule"], "match": f["match"]} for f in findings if f["level"] == "advisory-free"]
    print(json.dumps(hard))
