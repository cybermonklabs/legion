#!/usr/bin/env python3
"""Drive one real disagreement from our own review record through the shipped CONCORD library.

Reads concord-input.json beside it (or the path given as argv[1]) and runs the library's own wire
and engine path: build a request, build a reply, read the reply back with wire.parse_reply, record
it with engine.add_anchor / add_candidate / add_ratings, then engine.advance, exactly as
protocols/concord/skills/concord-initiator/scripts/concord_initiator.py does in _apply_reply.

Nothing here reimplements a protocol rule: every decision is the library's.
"""
import json
import sys
from pathlib import Path

from ioc_concord import __version__
from ioc_concord.engine import add_anchor, add_candidate, add_ratings, advance, column, leximin
from ioc_concord.schema import check_candidate
from ioc_concord.state import new_run_state
from ioc_concord.wire import (build_anchor_msg, build_anchor_reply, build_candidate_reply, build_commit_msg,
                              build_repair_msg, build_score_msg, build_score_reply, build_seed_msg, parse_reply)


def rule(title):
    print("\n" + "=" * 78)
    print(title)
    print("=" * 78)


def apply_reply(st, agent, text):
    """parse_reply, then the matching engine call, then advance — the initiator's order."""
    r = parse_reply(text)
    if r["kind"] == "anchor":
        add_anchor(st, agent, r["raw_reject"], r["raw_ideal"])
    elif r["kind"] == "candidate":
        add_candidate(st, agent, r["candidate"], r["rationale"], r.get("id"))
    else:
        add_ratings(st, agent, [(s["id"], s["raw"], s["reasoning"]) for s in r["scores"]])
    advance(st)
    return r


def run_one(cfg, run):
    agents = cfg["agents"]
    spec = cfg["schema"]
    rule(f"RUN {run['name']}: satisfaction_threshold={run['satisfaction_threshold']} "
         f"eps={run['eps']} B={run['B']}")

    st = new_run_state(agents, cfg["context"], cfg["x_reject"], cfg["x_ideal"], spec,
                       satisfaction_threshold=run["satisfaction_threshold"], eps=run["eps"], B=run["B"],
                       state_id=cfg["state_id"])

    print("\n-- the schema as the library renders it for the agents --")
    print(st["schema"])
    print("\n-- every candidate of the input, checked against the schema by schema.check_candidate --")
    for text in spec["enum"]:
        print(f"  {check_candidate(spec, text) or 'ok'}: {text}")

    print("\n-- ANCHOR request (wire.build_anchor_msg) --")
    print(build_anchor_msg(st, agents))
    for a in agents:
        anc = cfg["anchors"][a]
        reply = build_anchor_reply(anc["raw_reject"], anc["raw_ideal"], agent=a, state=st["state_id"])
        print(f"\n-- {a} anchor reply --")
        print(reply)
        apply_reply(st, a, reply)

    print("\n-- SEED request (wire.build_seed_msg) --")
    print(build_seed_msg(st, agents))
    for seed in cfg["seeds"]:
        reply = build_candidate_reply(seed["candidate"], seed["rationale"], cid=f"seed-{seed['agent']}",
                                      agent=seed["agent"], state=st["state_id"])
        print(f"\n-- {seed['agent']} seed reply --")
        print(reply)
        apply_reply(st, seed["agent"], reply)

    print("\n-- SCORE request (wire.build_score_msg) --")
    print(build_score_msg(st, agents))
    for a in agents:
        scores = [(c["id"], cfg["raw"][a][c["candidate"]], cfg["reasoning"][a][c["candidate"]]) for c in st["pool"]]
        reply = build_score_reply(scores, agent=a, state=st["state_id"])
        print(f"\n-- {a} score reply --")
        print(reply)
        apply_reply(st, a, reply)

    print("\n-- the engine after the seed round --")
    report(st)

    if st.get("pending") and run["feed_repair"]:
        b = st["pending"]["agent"]
        print(f"\n-- REPAIR request to the bottleneck {b} (wire.build_repair_msg) --")
        print(build_repair_msg(st, b))
        rep = cfg["repair"]
        cid = f"r{st['pending']['round']}-{rep['agent']}"
        reply = build_candidate_reply(rep["candidate"], rep["rationale"], cid=cid, agent=rep["agent"],
                                      state=st["state_id"])
        print(f"\n-- {rep['agent']} repair reply --")
        print(reply)
        apply_reply(st, rep["agent"], reply)
        for a in agents:
            reply = build_score_reply([(cid, cfg["raw"][a][rep["candidate"]], cfg["reasoning"][a][rep["candidate"]])],
                                      agent=a, state=st["state_id"])
            print(f"\n-- {a} score reply for {cid} --")
            print(reply)
            apply_reply(st, a, reply)
        print("\n-- the engine after the repair round --")
        report(st)

    if st["outcome"]:
        print("\n-- COMMIT (wire.build_commit_msg) --")
        print(build_commit_msg(st, st["outcome"]))
    print(f"\nOUTCOME {run['name']}: outcome={st['outcome']!r} winner={st['x_star']!r} rounds_spent={st['t']}")
    return st


def report(st):
    for c in st["pool"]:
        col = column(st, c["id"])
        shown = {a: (None if v is None else round(v, 6)) for a, v in col.items()}
        print(f"  {c['id']:<38} normalized={shown} leximin={[round(v, 6) for v in leximin(st, c['id'])]}")
    print(f"  x_star={st['x_star']!r} t={st['t']} pending={st['pending']} outcome={st['outcome']!r}")
    for e in st["log"]:
        print(f"  log: {e}")


def main():
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).with_name("concord-input.json")
    doc = json.loads(path.read_text(encoding="utf-8"))
    print(f"ioc_concord.__version__ = {__version__}")
    print(f"input = {path.name}")
    print(f"provenance = {json.dumps(doc['provenance'], sort_keys=True)}")
    print(f"derivation = {json.dumps(doc['derivation'], sort_keys=True)}")
    for run in doc["runs"]:
        run_one(doc["run_common"], run)


if __name__ == "__main__":
    main()
