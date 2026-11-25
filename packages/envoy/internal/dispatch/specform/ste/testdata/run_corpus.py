"""Print or record ste-lint's hard findings for each line of a corpus file.

Usage: python3 run_corpus.py [--write] [corpus.jsonl]

Each line of the corpus is one JSON string. For each line, in order, this gives one JSON line:
the list of {"rule", "match"} of the hard ("advisory-free") findings that ste-lint.py, beside
this file, gives that string, in ste-lint's order. It prints them. With --write it writes them to
corpus-findings.jsonl beside this file instead: the golden data that
TestPortMatchesSteLintOnTheCorpus compares with the Go port's findings. Run it with --write after
a change to the corpus or to ste-lint.py.
"""
import importlib.util
import json
import pathlib
import sys

here = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("ste_lint", here / "ste-lint.py")
ste = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ste)

args = sys.argv[1:]
write = "--write" in args
args = [arg for arg in args if arg != "--write"]
corpus = pathlib.Path(args[0]) if args else here / "corpus.jsonl"
with open(corpus, encoding="utf-8", newline="") as source:
    lines = source.read().split("\n")
if lines and lines[-1] == "":
    lines.pop()
answers = []
for line in lines:
    findings, _ = ste.lint(json.loads(line))
    hard = [{"rule": f["rule"], "match": f["match"]} for f in findings if f["level"] == "advisory-free"]
    answers.append(json.dumps(hard) + "\n")
if write:
    with open(here / "corpus-findings.jsonl", "w", encoding="utf-8", newline="") as golden:
        golden.writelines(answers)
else:
    sys.stdout.writelines(answers)
