#!/usr/bin/env python3
"""Re-run ACCORD's own term-similarity scorer over nine definition pairs.

Both backends of ACCORD's `Similarity` class are exercised:

  * the difflib backend, forced with ACCORD_EMBEDDING=off;
  * the sentence-transformer backend, with ACCORD_EMBEDDING_MODEL pointed at a
    model directory on disk (the class loads with local_files_only=True).

Nothing here re-implements the scoring: the class is imported from the pinned
clone and called.  The verdict words are ACCORD's own bands, read from the same
file: >= SIM_MERGE is `agreed`, [SIM_CLARIFY, SIM_MERGE) is `clarify`, below
SIM_CLARIFY is `flagged`.

Usage:
    ACCORD_SRC=$IOC_CLONES/cognition-protocols \
    ACCORD_MODEL_DIR=<dir or empty> python3 similarity.py
"""

from __future__ import annotations

import hashlib
import importlib.util
import os
import sys
from pathlib import Path

CLONE = Path(os.environ.get("ACCORD_SRC", "/legion/scratch/tmp/ioc-clones/cognition-protocols"))
INITIATOR = CLONE / "protocols/accord/skills/accord-initiator/scripts/accord_initiator.py"
TERMS = CLONE / "protocols/accord/mce-metrics/src/accord_mce_metrics/terms.py"
MODEL_DIR = os.environ.get("ACCORD_MODEL_DIR", "").strip()


def load(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


ai = load(INITIATOR, "accord_initiator_under_test")
terms = load(TERMS, "accord_mce_terms_under_test")

MERGE = ai.SIM_MERGE
CLARIFY = ai.SIM_CLARIFY


def verdict(score: float) -> str:
    if score >= MERGE:
        return "agreed"
    if score >= CLARIFY:
        return "clarify"
    return "flagged"


# (id, truth, left, right, provenance)
PAIRS = [
    ("p1", "differs by 100x", "fraction 0-1", "fraction 0-100",
     "named in the task"),
    ("p2", "differs by 1000x", "p99 latency in milliseconds", "p99 latency in seconds",
     "named in the task"),
    ("p3", "inverted", "severity 1 is most severe", "severity 1 is least severe",
     "named in the task"),
    ("p4", "different acts", "approved means a person approved this version", "approved means CI passed",
     "named in the task"),
    ("p5", "the same", "the deploy is the merge to main", "a merge to main is the deploy",
     "named in the task"),
    ("p6", "different events",
     "done means the change is driven in production",
     "done means the issue's last workflow phase finished",
     "ours: skills/dispatch/SKILL.md:45 against skills/legion-controller/SKILL.md:50-51"),
    ("p7", "inverted",
     "unresponsive means the session has not answered yet",
     "unresponsive means the session ended",
     "ours: skills/envoy/SKILL.md:49"),
    ("p8", "inverted",
     "a parked child is stopped, not done",
     "a parked child is done",
     "ours: skills/legion-architect/SKILL.md:45"),
    ("p9", "inverted",
     "from is the sender's own claim",
     "from is proof of who wrote the message",
     "ours: skills/envoy/SKILL.md:35"),
]


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main() -> int:
    print("# ACCORD term similarity, re-run")
    print(f"clone: {CLONE}")
    print(f"scorer: {INITIATOR.relative_to(CLONE)} class Similarity (lines 96-139)")
    print(f"thresholds read from the scorer: SIM_MERGE={MERGE} SIM_CLARIFY={CLARIFY} "
          f"GROUP_THRESHOLD={ai.GROUP_THRESHOLD}")
    print(f"mce-metrics threshold: terms.SIM_MERGE={terms.SIM_MERGE} "
          f"backend label={terms.BUILTIN_BACKEND!r}")
    scorer, thr, label = terms.load_scorer(None)
    print(f"terms.load_scorer(None) -> ({scorer.__name__}, {thr}, {label!r})")
    print()

    # The class reads ACCORD_EMBEDDING at call time, so each backend scores in
    # its own pass with the environment set for that pass only.
    os.environ["ACCORD_EMBEDDING"] = "off"
    os.environ.pop("ACCORD_EMBEDDING_MODEL", None)
    sim_difflib = ai.Similarity()
    assert sim_difflib._get_embedder() is None, "difflib pass must have no embedder"
    difflib_scores = {pid: sim_difflib(a, b) for pid, _t, a, b, _p in PAIRS}
    # Cross-check against the mce-metrics scorer, which has this backend only.
    for pid, _t, a, b, _p in PAIRS:
        assert abs(difflib_scores[pid] - scorer(a, b)) < 1e-12, pid
    print("difflib backend: ACCORD_EMBEDDING=off, scores equal "
          "accord_mce_metrics.terms.string_similarity for every pair")

    model_scores: dict[str, float] = {}
    if MODEL_DIR:
        os.environ["ACCORD_EMBEDDING"] = "on"
        os.environ["ACCORD_EMBEDDING_MODEL"] = MODEL_DIR
        sim_model = ai.Similarity()
        # Prove the embedder actually loaded; the class swallows every failure.
        if sim_model._get_embedder() is None:
            model_note = "embedder did not load (the class swallowed the error)"
        else:
            model_note = f"loaded {MODEL_DIR}"
            model_scores = {pid: sim_model(a, b) for pid, _t, a, b, _p in PAIRS}
    else:
        model_note = "no model directory given"
    print(f"model backend: {model_note}")
    if MODEL_DIR:
        for name in ("model.safetensors", "pytorch_model.bin"):
            f = Path(MODEL_DIR) / name
            if f.is_file():
                print(f"model file: {name} sha256={sha256(f)} bytes={f.stat().st_size}")
    print()

    rows = []
    for pid, truth, a, b, prov in PAIRS:
        rows.append((pid, truth, a, b, prov, difflib_scores[pid], model_scores.get(pid)))

    print("| pair | the truth | difflib score | difflib verdict | model score | model verdict | the pair |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for pid, truth, a, b, prov, ds, ms in rows:
        mcol = f"{ms:.3f}" if ms is not None else "n/a"
        mver = verdict(ms) if ms is not None else "n/a"
        print(f"| {pid} | {truth} | {ds:.3f} | {verdict(ds)} | {mcol} | {mver} | "
              f"`{a}` / `{b}` |")
    print()

    print("## machine readable")
    for pid, truth, a, b, prov, ds, ms in rows:
        mcol = f"{ms:.6f}" if ms is not None else "n/a"
        mver = verdict(ms) if ms is not None else "n/a"
        print(f"{pid} truth={truth} difflib={ds:.6f} difflib_verdict={verdict(ds)} "
              f"model={mcol} model_verdict={mver} from={prov}")
    print()

    merged_difflib = sum(1 for r in rows if verdict(r[5]) == "agreed" and r[1] != "the same")
    print(f"difflib: pairs that are not the same yet scored agreed: {merged_difflib}")
    if model_scores:
        merged_model = sum(1 for r in rows if verdict(r[6]) == "agreed" and r[1] != "the same")
        print(f"model: pairs that are not the same yet scored agreed: {merged_model}")
    for r in [r for r in rows if r[1] == "the same"]:
        print(f"{r[0]} is the same pair: difflib verdict {verdict(r[5])}", end="")
        print(f", model verdict {verdict(r[6])}" if model_scores else "")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
