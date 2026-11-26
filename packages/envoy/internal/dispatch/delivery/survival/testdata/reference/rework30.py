#!/usr/bin/env python3
# rework30.py -- scripts/15_rework.py of dispatch://INFRA-3117/artifact/survival-scripts-tar-gz@v5,
# copied for INFRA-3206's check k5 with three additions:
#   1. one line, right after the line that skips a remover that is no Legion merge, skips a
#      remover that came more than 30 days after the adding merge;
#   2. --no-30d turns that line off, and the copy then reads as 15_rework.py itself: on
#      14_keys.py's keys it gives 15_rework.json's buckets and distinct_pairs;
#   3. every pair the join keeps (past both lines) is written to <outdir>/rework30_pairs.tsv,
#      one row a pair: adder sha, remover sha, case (same, different, unresolved), lines,
#      gap in days.
# Its outputs are named rework30.* where 15_rework.py's are 15_rework.*.
#
#   rework30.py <datadir> <outdir> [--no-30d]
#
# It reads <datadir> as 15_rework.py does, through replaylib.py (replay_commits.tsv,
# replay_added_cat.tsv, replay_deaths_cat.tsv, merge_keys.tsv), and loads replaylib.py from its
# own directory as 15_rework.py does, so it runs from a directory holding it beside the
# archive's scripts/replaylib.py: prepare.sh links both into <dir>/bin.
# Writes <outdir>/rework30.txt, <outdir>/rework30.json and <outdir>/rework30_pairs.tsv.
#
# Its diff against 15_rework.py, of this file without lines 2 to the end-of-header line (lines
# whose text is blank lose their trailing space here):
#   diff -u --label 15_rework.py --label rework30.py 15_rework.py \
#     <(sed '2,/^# -- end of header --$/d' rework30.py)
#
# --- 15_rework.py
# +++ rework30.py
# @@ -21,6 +21,10 @@
#  from collections import defaultdict
#
#  DATA, OUT = sys.argv[1], sys.argv[2]
# +# --no-30d turns the 30-day line below off: the copy then reads as 15_rework.py itself
# +if sys.argv[3:] not in ([], ["--no-30d"]):
# +    sys.exit("usage: rework30.py <datadir> <outdir> [--no-30d]")
# +THIRTY = sys.argv[3:] != ["--no-30d"]
#  spec = importlib.util.spec_from_file_location(
#      "replaylib", f"{__file__.rsplit('/', 1)[0]}/replaylib.py")
#  rl = importlib.util.module_from_spec(spec)
# @@ -69,6 +73,7 @@
#      why = defaultdict(int)          # which side was unresolved
#      gaps = defaultdict(list)        # bucket -> [(gap days, lines)]
#      pairs = defaultdict(int)        # (adding key, removing key) -> lines
# +    kept = []                       # (adder sha, remover sha, case, lines, gap days)
#      same_project = 0
#      dead_total = [0, 0]             # every replay-dead cohort line, any killer
#      killed_by_legion = [0, 0]
# @@ -81,6 +86,7 @@
#              dead_total[1] += ng
#              if not (di in is_merge and grp[di] == "legion"):
#                  continue
# +            if THIRTY and (r.commits[di - 1][1] - mts) / DAY > 30: continue
#              killed_by_legion[0] += n
#              killed_by_legion[1] += ng
#              ka, kb = key_of[i][0], key_of[di][0]
# @@ -95,6 +101,8 @@
#              buckets[b][0] += n
#              buckets[b][1] += ng
#              gaps[b].append(((r.commits[di - 1][1] - mts) / DAY, n))
# +            kept.append((r.commits[i - 1][0], r.commits[di - 1][0], b, n,
# +                         (r.commits[di - 1][1] - mts) / DAY))
#
#      say("== Q1  Legion lines killed by a Legion merge: iteration, or rework?")
#      say(f"cohort merges {sum(1 for m in r.merges if m[3] == 'legion')}, "
# @@ -167,8 +175,11 @@
#                 "histogram_edges": EDGES,
#                 "top_pairs": [{"add": a, "remove": b, "lines": n} for (a, b), n in top],
#                 "distinct_pairs": len(pairs)},
# -              open(f"{OUT}/15_rework.json", "w"), indent=1)
# -    open(f"{OUT}/15_rework.txt", "w").write("\n".join(lines) + "\n")
# +              open(f"{OUT}/rework30.json", "w"), indent=1)
# +    open(f"{OUT}/rework30.txt", "w").write("\n".join(lines) + "\n")
# +    with open(f"{OUT}/rework30_pairs.tsv", "w") as fh:
# +        for a, d, b, n, gap in kept:
# +            fh.write(f"{a}\t{d}\t{b}\t{n}\t{gap}\n")
#
#
#  if __name__ == "__main__":
# -- end of header --
"""15_rework.py -- question 1: when a Legion merge kills a Legion line, is it
the same task iterating, or another task rewriting work already landed?

The replay already holds, for every (adding commit, removing commit) pair, how
many lines that pair killed, so this is a join of those rows against the issue
key 14_keys.py resolved for each merge -- no tree is blamed again.

Each Legion-killed Legion line falls in exactly one of:
  same       both merges resolve to one key: iteration inside one task;
  different  both resolve, to different keys: one tree rewriting another's;
  unresolved at least one side has no key -- counted apart, never folded into
             `different`, because an unresolved key may well be the same key.

Usage: 15_rework.py <datadir> <outdir>
Writes <outdir>/15_rework.txt and <outdir>/15_rework.json
"""
import importlib.util
import json
import sys
from collections import defaultdict

DATA, OUT = sys.argv[1], sys.argv[2]
# --no-30d turns the 30-day line below off: the copy then reads as 15_rework.py itself
if sys.argv[3:] not in ([], ["--no-30d"]):
    sys.exit("usage: rework30.py <datadir> <outdir> [--no-30d]")
THIRTY = sys.argv[3:] != ["--no-30d"]
spec = importlib.util.spec_from_file_location(
    "replaylib", f"{__file__.rsplit('/', 1)[0]}/replaylib.py")
rl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rl)
DAY = rl.DAY


def quantiles_weighted(pairs, qs):
    """pairs = [(value, weight)]; line-weighted quantiles"""
    pairs = sorted(pairs)
    total = sum(w for _v, w in pairs)
    out = []
    for q in qs:
        want = q * total
        run = 0.0
        val = pairs[-1][0] if pairs else 0.0
        for v, w in pairs:
            run += w
            if run >= want:
                val = v
                break
        out.append(val)
    return out


def main():
    r = rl.Replay(DATA)
    lines = []

    def say(s=""):
        lines.append(s)
        print(s)

    is_merge = {i for i in range(1, len(r.commits) + 1) if r.commits[i - 1][2]}
    grp = {i: r.commits[i - 1][3] for i in range(1, len(r.commits) + 1)}
    key_of = {i: r.keys.get(r.commits[i - 1][0], (None, "none"))
              for i in range(1, len(r.commits) + 1)}

    legion_added = sum(rl.take(a, rl.ALL) for i, _s, _t, g, a in r.merges
                       if g == "legion")
    legion_added_exgen = sum(rl.take(a, rl.EX_GEN) for i, _s, _t, g, a in r.merges
                             if g == "legion")

    # ---- the join ----------------------------------------------------------
    buckets = {"same": [0, 0], "different": [0, 0], "unresolved": [0, 0]}
    why = defaultdict(int)          # which side was unresolved
    gaps = defaultdict(list)        # bucket -> [(gap days, lines)]
    pairs = defaultdict(int)        # (adding key, removing key) -> lines
    kept = []                       # (adder sha, remover sha, case, lines, gap days)
    same_project = 0
    dead_total = [0, 0]             # every replay-dead cohort line, any killer
    killed_by_legion = [0, 0]
    for i, _sha, mts, g, _a in r.merges:
        if g != "legion":
            continue
        for di, c in r.deaths.get(i, ()):
            n, ng = rl.take(c, rl.ALL), rl.take(c, rl.EX_GEN)
            dead_total[0] += n
            dead_total[1] += ng
            if not (di in is_merge and grp[di] == "legion"):
                continue
            if THIRTY and (r.commits[di - 1][1] - mts) / DAY > 30: continue
            killed_by_legion[0] += n
            killed_by_legion[1] += ng
            ka, kb = key_of[i][0], key_of[di][0]
            b = rl.classify(ka, kb)
            if b == "unresolved":
                why["adder only" if kb is not None else
                    ("remover only" if ka is not None else "both sides")] += n
            elif b == "different":
                pairs[(ka, kb)] += n
                if ka.split("-")[0] == kb.split("-")[0]:
                    same_project += n
            buckets[b][0] += n
            buckets[b][1] += ng
            gaps[b].append(((r.commits[di - 1][1] - mts) / DAY, n))
            kept.append((r.commits[i - 1][0], r.commits[di - 1][0], b, n,
                         (r.commits[di - 1][1] - mts) / DAY))

    say("== Q1  Legion lines killed by a Legion merge: iteration, or rework?")
    say(f"cohort merges {sum(1 for m in r.merges if m[3] == 'legion')}, "
        f"lines added {legion_added} (ex-generated {legion_added_exgen}); "
        f"of those, replay-dead at the tip {dead_total[0]}, "
        f"killed by a cohort merge {killed_by_legion[0]}")
    say(f"{'case':10} | {'lines':>9} | {'of Legion-killed':>16} | "
        f"{'of all Legion added':>19} | {'ex-gen lines':>12} | {'of ex-gen added':>15}")
    for b in ("same", "different", "unresolved"):
        n, ng = buckets[b]
        say(f"{b:10} | {n:9d} | {rl.pct(n, killed_by_legion[0]):>16} | "
            f"{rl.pct(n, legion_added):>19} | {ng:12d} | "
            f"{rl.pct(ng, legion_added_exgen):>15}")
    say(f"  unresolved breaks down as: "
        + ", ".join(f"{k} {v}" for k, v in sorted(why.items())))
    say(f"  of the `different` lines, {same_project} "
        f"({rl.pct(same_project, buckets['different'][0]).strip()}) were removed by a "
        f"merge of the same project (INFRA/LEGION), the rest across projects")

    # ---- how long between the two merges ------------------------------------
    say("\n-- gap in days between the adding merge and the removing merge "
        "(line-weighted)")
    say(f"{'case':10} | {'lines':>9} | {'p10':>6} | {'p25':>6} | {'median':>7} | "
        f"{'p75':>7} | {'p90':>7} | {'mean':>7}")
    qres = {}
    for b in ("same", "different", "unresolved"):
        if not gaps[b]:
            continue
        q = quantiles_weighted(gaps[b], (0.10, 0.25, 0.50, 0.75, 0.90))
        tot = sum(w for _v, w in gaps[b])
        mean = sum(v * w for v, w in gaps[b]) / tot
        qres[b] = {"p10": q[0], "p25": q[1], "median": q[2], "p75": q[3],
                   "p90": q[4], "mean": mean, "lines": tot}
        say(f"{b:10} | {tot:9d} | {q[0]:6.2f} | {q[1]:6.2f} | {q[2]:7.2f} | "
            f"{q[3]:7.2f} | {q[4]:7.2f} | {mean:7.2f}")

    say("\n-- the same gap as a histogram (share of that case's lines)")
    EDGES = [(0, 1), (1, 2), (2, 7), (7, 14), (14, 30), (30, 1e9)]
    say(f"{'case':10} | " + " | ".join(
        f"{('<1d' if lo == 0 else ('30d+' if hi > 1e8 else f'{lo}-{hi}d')):>9}"
        for lo, hi in EDGES))
    hist = {}
    for b in ("same", "different", "unresolved"):
        if not gaps[b]:
            continue
        tot = sum(w for _v, w in gaps[b])
        row = []
        for lo, hi in EDGES:
            n = sum(w for v, w in gaps[b] if lo <= v < hi)
            row.append(n)
        hist[b] = row
        say(f"{b:10} | " + " | ".join(f"{rl.pct(n, tot):>9}" for n in row))
        say(f"{'':10} | " + " | ".join(f"{n:9d}" for n in row))

    say("\n-- the ten (adding issue -> removing issue) pairs holding the most lines")
    say(f"{'adding issue':14} -> {'removing issue':14} | {'lines':>8} | "
        f"{'of different':>12} | {'of all Legion added':>19}")
    top = sorted(pairs.items(), key=lambda kv: -kv[1])[:10]
    for (ka, kb), n in top:
        say(f"{ka:14} -> {kb:14} | {n:8d} | "
            f"{rl.pct(n, buckets['different'][0]):>12} | {rl.pct(n, legion_added):>19}")
    say(f"distinct (adding, removing) key pairs: {len(pairs)}")

    json.dump({"legion_added": legion_added,
               "legion_added_ex_gen": legion_added_exgen,
               "dead_total": dead_total[0], "killed_by_legion": killed_by_legion[0],
               "buckets": buckets, "unresolved_sides": dict(why),
               "same_project_lines": same_project,
               "gap_quantiles": qres, "gap_histogram": hist,
               "histogram_edges": EDGES,
               "top_pairs": [{"add": a, "remove": b, "lines": n} for (a, b), n in top],
               "distinct_pairs": len(pairs)},
              open(f"{OUT}/rework30.json", "w"), indent=1)
    open(f"{OUT}/rework30.txt", "w").write("\n".join(lines) + "\n")
    with open(f"{OUT}/rework30_pairs.tsv", "w") as fh:
        for a, d, b, n, gap in kept:
            fh.write(f"{a}\t{d}\t{b}\t{n}\t{gap}\n")


if __name__ == "__main__":
    main()
