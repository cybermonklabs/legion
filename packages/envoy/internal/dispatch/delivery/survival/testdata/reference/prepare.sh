#!/usr/bin/env bash
# prepare.sh <dir> -- the reference inputs and outputs of INFRA-3206's check k5: what Dispatch's
# code-survival replay (internal/dispatch/delivery/survival) is compared with by
# reference_test.go (build tag survivalref, SURVIVAL_REF_DIR=<dir>). Everything is written under
# <dir>; the repository is only read (git log, rev-list, rev-parse, cat-file on its objects).
#
#   bash prepare.sh <dir>
#
# Five steps, one line printed each. A step whose outputs exist is skipped and its line printed
# again from <dir>/steps/<n>.txt; a step stopped part way resumes, keeping what it fetched (the
# archives, each issue read, each diff of the second repository, the GitHub search slices), so a second run
# reads nothing over the network.
#
# The repository is $LEGION_WORKSPACE, else the checkout holding this script. Needs git, gh
# (signed in; reads only), curl, jq, python3, the dispatch CLI, DISPATCH_URL with
# DISPATCH_TOKEN_FILE for step 1 (the token goes to curl on stdin as a header, never printed), and
# AI_REPO, the owner/name of the second repository, for step 5 when a diff is missing.
#
# 1. The archives of INFRA-3117. Reads GET $DISPATCH_URL/api/v1/issues/INFRA-3117/artifacts/
#    <slug>/versions/<n> for survival-scripts-tar-gz v5, feasibility-second-repo-output-tar-gz v1
#    and survival-pass-output-tar-gz v2. Writes <dir>/archives/<slug>.tar.gz and, unpacked,
#    <dir>/archives/<slug>/ (scripts/ and scripts-feasibility/; feasibility-second-repo/; out/).
# 2. cybermonklabs/legion's commits. Reads the repository's
#    `git log --reverse --first-parent -p -U0 -M` of aa6395134b37~1..c0c4997971c1, the command
#    replay.py git runs, and the archive's lg/shas.txt. Writes <dir>/lg/git/<sha>.diff.gz (each
#    commit's part of that output as git writes it, without its `\x01<sha>` line),
#    <dir>/lg/commits.txt (`%H %cI %P %s`, chain order) and <dir>/lg/shas.txt (the archive's).
#    Fails unless the chain is lg/shas.txt's 3,356 commits in its order.
# 3. The pull requests and the issue each names. Reads GitHub's closed pull requests, newest
#    update first, up to the page holding one updated before 2026-08-01, keeping those into main
#    and those into another branch whose merge commit is on step 2's chain (a stacked pull
#    request: base=main alone misses 4 of the chain's merges), then `dispatch read --issue` of
#    every key a merged one's head branch, title or body names (the key pattern of
#    delivery/intake.go, issueKeyCandidate). Writes <dir>/lg/pulls.json (number,
#    merge_commit_sha, head.ref, base.ref, user.login, title, body, merged_at of each updated
#    since 2026-08-01), <dir>/lg/issue-cache/<key>.txt (the read) or <key>.missing (no such issue),
#    <dir>/lg/issues.tsv (key, project, repository: every issue read), <dir>/lg/pr_issue.tsv
#    (merge sha, number, issue or empty: every merged pull request, by attribution's sources 3
#    and 4 held to cybermonklabs/legion) and <dir>/lg/merge_keys.tsv (the same issue in
#    14_keys.py's shape, sha number group key source, for each merge of step 2's chain).
# 4. The scripts' own outputs, in the layout scripts-feasibility/headline.sh builds. Reads steps
#    1 to 3, the repository and GitHub's search (01_fetch_prs.sh, kept in its slices/). Runs
#    01_fetch_prs.sh, 02_cohort.py, commits_tsv.py (replay_commits.tsv, which must be the
#    archive's lg/commits.tsv), replay.py git, 14_keys.py, 15_rework.py, 16_window.py, and
#    rework30.py twice: with its 30-day line off on 14_keys.py's keys, and on step 3's keys.
#    Writes <dir>/lg/r_git_added.tsv, r_git_deaths.tsv, r_git.json (the replay),
#    <dir>/lg/an/git/data/ and out/ (headline.sh's data and out), <dir>/lg/an/git/rework30-off/,
#    <dir>/lg/an/k5/data/ (links) and out/, <dir>/bin/ (rework30.py beside replaylib.py) and
#    <dir>/lg/an/checks.txt. Fails unless the replay's per-commit output is the archive's
#    lg/r_git_added.tsv and lg/r_git_deaths.tsv, and rework30.py --no-30d gives 15_rework.py's
#    own output and 15_rework.json's buckets and distinct_pairs.
# 5. The second repository's diffs (AI_REPO). Reads the archive's ai/shas.txt and, for
#    each diff not on disk, GitHub's diff route through scripts-feasibility/fetch_diff.py in its gh
#    mode (PF_REPO=$AI_REPO, GH_TOKEN unset). Writes <dir>/ai/shas.txt,
#    <dir>/ai/diffs/<xx>/<sha>.diff.gz, <dir>/ai/diffs/stats.jsonl and <dir>/ai/fetch.log.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: prepare.sh <dir>" >&2; exit 2; }
mkdir -p "$1"
DIR="$(cd "$1" && pwd)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${LEGION_WORKSPACE:-$(git -C "$HERE" rev-parse --show-toplevel)}"
FIRST=aa6395134b37e3e0e267282f7444a9d6e122f0e3
TIP=c0c4997971c1dadd9f7d73347a2db3266206cf28
RANGE="$FIRST~1..$TIP"
SS="$DIR/archives/survival-scripts-tar-gz"
FS="$DIR/archives/feasibility-second-repo-output-tar-gz/feasibility-second-repo"
SP="$DIR/archives/survival-pass-output-tar-gz/out"
# the archive's scripts are run in place; keep their bytecode out of <dir>
export PYTHONDONTWRITEBYTECODE=1
mkdir -p "$DIR/steps"
git -C "$REPO" cat-file -e "$TIP^{commit}"

fail() { echo "prepare.sh: $*" >&2; exit 1; }

# kept <n> <output>...: prints step n's line again and succeeds when the step finished before
# and each of its outputs exists
kept() {
  local n="$1" f
  shift
  [ -s "$DIR/steps/$n.txt" ] || return 1
  for f in "$@"; do
    [ -e "$f" ] || return 1
  done
  echo "$(cat "$DIR/steps/$n.txt") [kept: done by an earlier run]"
}

# finish <n> <line>: records step n's line and prints it
finish() {
  echo "$2" > "$DIR/steps/$1.txt"
  echo "$2"
}

step1() {
  kept 1 "$SS/scripts/15_rework.py" "$FS/README.txt" "$SP/15_rework.json" && return
  local sv slug v tgz out got=0 sizes=""
  mkdir -p "$DIR/archives"
  for sv in survival-scripts-tar-gz:5 feasibility-second-repo-output-tar-gz:1 \
    survival-pass-output-tar-gz:2; do
    slug="${sv%:*}"
    v="${sv#*:}"
    tgz="$DIR/archives/$slug.tar.gz"
    out="$DIR/archives/$slug"
    if [ ! -s "$tgz" ]; then
      printf 'Authorization: Bearer %s\n' "$(cat "${DISPATCH_TOKEN_FILE:?set DISPATCH_TOKEN_FILE}")" |
        curl -sSfL --retry 3 -H @- -o "$tgz.part" \
          "${DISPATCH_URL:?set DISPATCH_URL}/api/v1/issues/INFRA-3117/artifacts/$slug/versions/$v"
      gzip -t "$tgz.part"
      mv "$tgz.part" "$tgz"
      got=$((got + 1))
    fi
    if [ ! -d "$out" ]; then
      rm -rf "$out.part"
      mkdir "$out.part"
      tar -xzf "$tgz" -C "$out.part"
      mv "$out.part" "$out"
    fi
    sizes="$sizes${sizes:+, }$slug v$v $(wc -c < "$tgz") bytes"
  done
  finish 1 "step 1: archives: 3 of 3 unpacked under $DIR/archives, $got downloaded now ($sizes)"
}

step2() {
  local git="$DIR/lg/git" commits="$DIR/lg/commits.txt" got
  kept 2 "$git" "$commits" "$DIR/lg/shas.txt" && return
  mkdir -p "$DIR/lg"
  cp "$FS/lg/shas.txt" "$DIR/lg/shas.txt"
  rm -rf "$git" "$git.part"
  mkdir "$git.part"
  # one gzipped file a commit, split where replay.py's git_stream splits (a line starting \x01)
  got="$(python3 - "$REPO" "$RANGE" "$git.part" "$DIR/lg/shas.txt" <<'PY'
import gzip
import subprocess
import sys

repo, rng, out, shas = sys.argv[1:5]
want = [line.strip() for line in open(shas) if line.strip()]
proc = subprocess.Popen(
    ["git", "-C", repo, "-c", "core.quotePath=false", "log", "--reverse", "--first-parent",
     "-p", "-U0", "-M", "--no-color", "--format=%x01%H", rng],
    stdout=subprocess.PIPE, bufsize=1 << 22)
seen, fh, size = [], None, 0
for line in proc.stdout:
    if line[:1] == b"\x01":
        if fh is not None:
            fh.close()
        seen.append(line[1:].rstrip(b"\n").decode())
        # mtime 0: the same commit always gives the same bytes
        fh = gzip.GzipFile(f"{out}/{seen[-1]}.diff.gz", "wb", compresslevel=6, mtime=0)
        continue
    assert fh is not None, "git log wrote a line before the first commit"
    fh.write(line)
    size += len(line)
if fh is not None:
    fh.close()
proc.wait()
assert proc.returncode == 0, proc.returncode
assert seen == want, f"the chain holds {len(seen)} commits, lg/shas.txt {len(want)}, or another order"
print(f"{len(seen)} {size}")
PY
)"
  git -C "$REPO" log --reverse --first-parent --format='%H %cI %P %s' "$RANGE" > "$commits.part"
  cut -d' ' -f1 "$commits.part" | cmp -s - "$DIR/lg/shas.txt" ||
    fail "step 2: commits.txt's commits are not lg/shas.txt's"
  [ "${got% *}" -eq 3356 ] || fail "step 2: ${got% *} commits where 3356 were expected"
  mv "$commits.part" "$commits"
  mv "$git.part" "$git"
  finish 2 "step 2: legion: ${got% *} commits of $RANGE, equal in order to lg/shas.txt, one gzipped diff each under $DIR/lg/git (${got#* } bytes of git log -p -U0 -M), chain in $DIR/lg/commits.txt"
}

# read_issue <cache> <key>: `dispatch read --issue <key>` kept as <key>.txt, or as <key>.missing
# where Dispatch has no such issue; any other failure is retried, then fails the step
read_issue() {
  local cache="$1" key="$2" out rc attempt
  if [ -e "$cache/$key.txt" ] || [ -e "$cache/$key.missing" ]; then
    return 0
  fi
  for attempt in 1 2 3 4; do
    rc=0
    out="$(dispatch read --issue "$key" 2>&1)" || rc=$?
    if [ "$rc" -eq 0 ] && grep -q '^Key: ' <<< "$out"; then
      printf '%s\n' "$out" > "$cache/$key.txt.part"
      mv "$cache/$key.txt.part" "$cache/$key.txt"
      return 0
    fi
    if [ "$rc" -eq 1 ] && [[ "$out" == NOT_FOUND:* ]]; then
      printf '%s\n' "$out" > "$cache/$key.missing"
      return 0
    fi
    sleep $((attempt * 5))
  done
  printf 'dispatch read --issue %s failed (exit %s): %s\n' "$key" "$rc" "$out" >&2
  return 1
}
export -f read_issue

# The attribution's sources 3 and 4 (delivery/attribution.go): the keys of the head branch,
# upper-cased, then those of the title and the body, each list deduplicated in first-seen order;
# a candidate is an issue Dispatch holds under that key, at most 25 of them, and the pull
# request's issue is the first whose repository is cybermonklabs/legion.
#   keys <pulls.json>                                     every key a merged one names, once
#   resolve <pulls.json> <cache> <commits.txt> <lgdir>    writes issues.tsv, pr_issue.tsv,
#                                                         merge_keys.tsv; prints the counts
STEP3_PY="$(cat <<'PY'
import json
import os
import re
import sys

# delivery/intake.go issueKeyCandidate; Go's \b is ASCII's
KEY = re.compile(r"\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b", re.ASCII)
BOUND = 25                      # delivery/attribution.go attributionCandidateBound
REPO = "cybermonklabs/legion"
LEGION_LOGINS = {"legion-implementer[bot]", "trajectory-s-legion-implementer[bot]"}


def unique_keys(text):
    out = []
    for key in KEY.findall(text):
        if key not in out:
            out.append(key)
    return out


def upper(text):
    """strings.ToUpper: each rune's single upper-case rune, where it has one"""
    return "".join(u if len(u := c.upper()) == 1 else c for c in text)


def candidates(p):
    return ([(k, "branch") for k in unique_keys(upper(p["head"]["ref"] or ""))]
            + [(k, "title_body")
               for k in unique_keys((p["title"] or "") + "\n" + (p["body"] or ""))])


mode, pulls = sys.argv[1], sys.argv[2]
merged = sorted((p for p in json.load(open(pulls)) if p["merged_at"]),
                key=lambda p: p["number"])
keys = list(dict.fromkeys(k for p in merged for k, _ in candidates(p)))
if mode == "keys":
    print("\n".join(keys))
    sys.exit()

cache, commits, lg = sys.argv[3:6]
issues, missing, moved = {}, 0, 0
for key in keys:
    if os.path.exists(f"{cache}/{key}.missing"):
        missing += 1
        continue
    text = open(f"{cache}/{key}.txt").read()
    if re.search(r"(?m)^Key: (\S+)", text).group(1) != key:
        moved += 1              # Dispatch answers it under another key: no issue holds this one
        continue
    repo = re.search(r"(?m)^Repository: (\S*)", text)
    repo = repo.group(1) if repo else ""
    issues[key] = (key.rsplit("-", 1)[0], "" if repo == "none" else repo)
with open(f"{lg}/issues.tsv", "w") as fh:
    for key in sorted(issues, key=lambda k: (k.rsplit("-", 1)[0], int(k.rsplit("-", 1)[1]))):
        fh.write(f"{key}\t{issues[key][0]}\t{issues[key][1]}\n")

window = {line.split(" ", 1)[0] for line in open(commits)}
picked, rows = {}, []
for p in merged:
    held = [c for c in candidates(p) if c[0] in issues][:BOUND]
    picked[p["number"]] = next((c for c in held if issues[c[0]][1].lower() == REPO), None)
with open(f"{lg}/pr_issue.tsv", "w") as fh:
    for p in merged:
        pick = picked[p["number"]]
        fh.write(f"{p['merge_commit_sha']}\t{p['number']}\t{pick[0] if pick else ''}\n")
with open(f"{lg}/merge_keys.tsv", "w") as fh:
    for p in merged:
        if p["merge_commit_sha"] not in window:
            continue
        pick = picked[p["number"]]
        legion = ((p["head"]["ref"] or "").startswith("legion/")
                  or (p["user"]["login"] or "") in LEGION_LOGINS)
        rows.append(legion)
        fh.write(f"{p['merge_commit_sha']}\t{p['number']}\t{'legion' if legion else 'other'}\t"
                 f"{pick[0] if pick else '-'}\t{pick[1] if pick else 'none'}\n")
by = [c[1] for c in picked.values() if c]
print(f"{len(json.load(open(pulls)))} closed pull requests updated since 2026-08-01 (into main, "
      f"or onto the chain from another base), "
      f"{len(merged)} merged, in lg/pulls.json; {len(keys)} keys named, "
      f"{len(issues)} issues read ({missing} no such issue, {moved} answered under another key), "
      f"{sum(1 for v in issues.values() if v[1].lower() == REPO)} of them in {REPO}, in "
      f"lg/issues.tsv; {len(by)} of {len(merged)} merged pull requests attributed "
      f"({by.count('branch')} by branch, {by.count('title_body')} by title or body), in "
      f"lg/pr_issue.tsv; {len(rows)} merges of the chain in lg/merge_keys.tsv "
      f"({sum(rows)} legion, {len(rows) - sum(rows)} other)")
PY
)"

# pulls <out> <commits.txt>: GitHub's closed pull requests, newest update first, each page read up
# to the one holding a pull request updated before 2026-08-01 (the window starts 2026-08-15), and
# the first page once more at the end, for one updated while the pages were read; kept are those
# into main and those into another branch whose merge commit is on the chain
pulls() {
  local out="$1" chain="$2" pages="$1.pages" page=1 n oldest
  local route="repos/cybermonklabs/legion/pulls?state=closed&sort=updated&direction=desc&per_page=100"
  local reduce='[.[] | {number, merge_commit_sha, head: {ref: .head.ref}, base: {ref: .base.ref}, user: {login: .user.login}, title, body, merged_at, updated_at}]'
  rm -rf "$pages"
  mkdir "$pages"
  while :; do
    gh api "$route&page=$page" --jq "$reduce" > "$pages/$page.json"
    n="$(jq 'length' "$pages/$page.json")"
    oldest="$(jq -r 'map(.updated_at) | min // ""' "$pages/$page.json")"
    if [ "$n" -lt 100 ] || [[ "$oldest" < "2026-08-01" ]]; then
      break
    fi
    page=$((page + 1))
  done
  gh api "$route&page=1" --jq "$reduce" > "$pages/0.json"
  jq -s --rawfile chain "$chain" '
    (reduce ($chain | split("\n")[] | select(length > 0) | split(" ")[0]) as $s ({}; .[$s] = true)) as $on
    | add | group_by(.number) | map(max_by(.updated_at))
    | map(select(.updated_at >= "2026-08-01"
        and (.base.ref == "main" or $on[.merge_commit_sha // ""] == true)) | del(.updated_at))
    | sort_by(.number)' "$pages"/*.json > "$out.part"
  mv "$out.part" "$out"
  rm -rf "$pages"
}

step3() {
  local lg="$DIR/lg" cache="$DIR/lg/issue-cache" line before after
  kept 3 "$lg/pulls.json" "$lg/issues.tsv" "$lg/pr_issue.tsv" "$lg/merge_keys.tsv" && return
  mkdir -p "$cache"
  [ -s "$lg/pulls.json" ] || pulls "$lg/pulls.json" "$lg/commits.txt"
  before="$(find "$cache" -name '*.txt' -o -name '*.missing' | wc -l)"
  python3 -c "$STEP3_PY" keys "$lg/pulls.json" |
    xargs -r -P 4 -I{} bash -c 'read_issue "$1" "$2"' _ "$cache" {}
  after="$(find "$cache" -name '*.txt' -o -name '*.missing' | wc -l)"
  line="$(python3 -c "$STEP3_PY" resolve "$lg/pulls.json" "$cache" "$lg/commits.txt" "$lg")"
  finish 3 "step 3: $line; $((after - before)) keys read from Dispatch now"
}

step4() {
  local A="$DIR/lg/an/git" K="$DIR/lg/an/k5" f
  local D="$A/data" O="$A/out"
  kept 4 "$DIR/lg/r_git.json" "$O/02_cohort.txt" "$O/14_keys.txt" "$O/15_rework.json" \
    "$O/16_window.json" "$A/rework30-off/rework30.json" "$K/out/rework30_pairs.tsv" \
    "$DIR/lg/an/checks.txt" && return
  mkdir -p "$D" "$O" "$A/rework30-off" "$K/data" "$K/out" "$DIR/bin"
  # run_all.sh's first two steps: the merged pull requests by GitHub search, and the cohort
  [ -s "$D/prs_raw.json" ] || bash "$SS/scripts/01_fetch_prs.sh" "$REPO" "$D" 2> "$D/01_fetch_prs.log"
  python3 "$SS/scripts/02_cohort.py" "$REPO" "$D" "$TIP" > "$O/02_cohort.txt"
  # replay_commits.tsv as 10_replay.py writes it: commits_tsv.py over step 2's chain and the cohort
  python3 - "$DIR/lg/commits.txt" "$D/commits.json" <<'PY'
import json
import sys

rows = [line.split(" ", 2)[:2] for line in open(sys.argv[1])]
json.dump({"list": [{"sha": sha, "date": date} for sha, date in reversed(rows)]},
          open(sys.argv[2], "w"))
PY
  python3 "$SS/scripts-feasibility/commits_tsv.py" "$D/commits.json" "$D/replay_commits.tsv" \
    "$D/cohort.json" > /dev/null
  cmp -s "$D/replay_commits.tsv" "$FS/lg/commits.tsv" ||
    fail "step 4: replay_commits.tsv is not the archive's lg/commits.tsv"
  # the replay, as second_repo.sh runs it for legion; r_git.json is the last file it writes
  [ -s "$DIR/lg/r_git.json" ] ||
    python3 "$SS/scripts-feasibility/replay.py" git "$REPO" "$D/replay_commits.tsv" \
      "$DIR/lg/r_git" "$SS/scripts" > /dev/null
  # headline.sh's data directory
  cp "$DIR/lg/r_git_added.tsv" "$D/replay_added_cat.tsv"
  cp "$DIR/lg/r_git_deaths.tsv" "$D/replay_deaths_cat.tsv"
  awk -F'\t' '{print $1"\t"$2"\t"($3+$4+$5+$6)"\t"($4+$6)}' \
    "$D/replay_added_cat.tsv" > "$D/replay_added.tsv"
  awk -F'\t' '{print $1"\t"$2"\t"($3+$4+$5+$6)"\t"($4+$6)}' \
    "$D/replay_deaths_cat.tsv" > "$D/replay_deaths.tsv"
  python3 "$SS/scripts/14_keys.py" "$REPO" "$D" "$O" > /dev/null
  python3 "$SS/scripts/15_rework.py" "$D" "$O" > /dev/null
  python3 "$SS/scripts/16_window.py" "$D" "$O" > /dev/null
  # rework30.py loads replaylib.py from its own directory, as 15_rework.py does
  ln -sfn ../archives/survival-scripts-tar-gz/scripts/replaylib.py "$DIR/bin/replaylib.py"
  ln -sfn "$HERE/rework30.py" "$DIR/bin/rework30.py"
  python3 "$DIR/bin/rework30.py" "$D" "$A/rework30-off" --no-30d > /dev/null
  for f in replay_commits.tsv replay_added_cat.tsv replay_deaths_cat.tsv; do
    ln -sfn "../../git/data/$f" "$K/data/$f"
  done
  ln -sfn ../../../merge_keys.tsv "$K/data/merge_keys.tsv"
  python3 "$DIR/bin/rework30.py" "$K/data" "$K/out" > /dev/null
  python3 - "$DIR" "$FS" "$SP" > "$DIR/lg/an/checks.txt.part" <<'PY'
import difflib
import json
import sys

DIR, FS, SP = sys.argv[1:4]
A, K = f"{DIR}/lg/an/git", f"{DIR}/lg/an/k5"
out, bad = [], []


def rows(path):
    return [line.rstrip("\n").split("\t") for line in open(path)]


def same(name, ok):
    out.append(f"{name}: {'same' if ok else 'DIFFERS'}")
    return ok


n = sum(1 for _ in open(f"{DIR}/lg/shas.txt"))
ours = {int(r[0]): r[1:] for r in rows(f"{DIR}/lg/r_git_added.tsv")}
theirs = {int(r[0]): r[1:] for r in rows(f"{FS}/lg/r_git_added.tsv")}
assert set(ours) | set(theirs) <= set(range(1, n + 1))
added = sum(1 for i in range(1, n + 1) if ours.get(i) != theirs.get(i))
ours = {(int(r[0]), int(r[1])): r[2:] for r in rows(f"{DIR}/lg/r_git_deaths.tsv")}
theirs = {(int(r[0]), int(r[1])): r[2:] for r in rows(f"{FS}/lg/r_git_deaths.tsv")}
pairs = set(ours) | set(theirs)
deaths = sum(1 for k in pairs if ours.get(k) != theirs.get(k))
out.append(f"replay.py git, added lines per commit beside the archive's lg/r_git_added.tsv: "
           f"{n} commits compared, {added} differ")
out.append(f"replay.py git, lines per (adder, remover) pair beside lg/r_git_deaths.tsv: "
           f"{len(pairs)} pairs compared, {deaths} differ")
if added or deaths:
    bad.append("the replay's per-commit output is not the archive's")
ours, theirs = json.load(open(f"{DIR}/lg/r_git.json")), json.load(open(f"{FS}/lg/r_git.json"))
ours.pop("seconds"), theirs.pop("seconds")
same("r_git.json beside the archive's (all but seconds)", ours == theirs)

off = json.load(open(f"{A}/rework30-off/rework30.json"))
if not same("rework30.py --no-30d beside 15_rework.py's own 15_rework.json",
            off == json.load(open(f"{A}/out/15_rework.json"))):
    bad.append("rework30.py --no-30d does not read as 15_rework.py")
ref = json.load(open(f"{SP}/15_rework.json"))
b = off["buckets"]
figures = (f"buckets different {b['different'][0]} / same {b['same'][0]} / "
           f"unresolved {b['unresolved'][0]}, distinct_pairs {off['distinct_pairs']}")
out.append(f"rework30.py --no-30d on 14_keys.py's keys: {figures}")
if not same("those beside the archive's 15_rework.json",
            (off["buckets"], off["distinct_pairs"]) == (ref["buckets"], ref["distinct_pairs"])):
    bad.append("rework30.py --no-30d does not give 15_rework.json's buckets and distinct_pairs")
archive = {}
for name, load in (("02_cohort.txt", lambda p: open(p).read()),
                   ("14_keys.txt", lambda p: open(p).read()),
                   ("15_rework.json", lambda p: json.load(open(p))),
                   ("16_window.json", lambda p: json.load(open(p)))):
    archive[name] = same(f"{name} beside the archive's out/{name}",
                         load(f"{A}/out/{name}") == load(f"{SP}/{name}"))
    if not archive[name] and name.endswith(".txt"):
        out.extend("  " + line for line in difflib.unified_diff(
            open(f"{SP}/{name}").read().splitlines(), open(f"{A}/out/{name}").read().splitlines(),
            "archive", "this run", n=0, lineterm=""))

merges = {r[1]: r[4] for r in rows(f"{A}/data/replay_commits.tsv") if r[3] == "1"}
keys = {r[0]: r for r in rows(f"{DIR}/lg/merge_keys.tsv")}
differ = (len(set(merges) - set(keys)), len(set(keys) - set(merges)),
          sum(1 for s in set(keys) & set(merges) if keys[s][2] != merges[s]))
out.append(f"step 3's merge_keys.tsv beside replay_commits.tsv's merges: {len(keys)} and "
           f"{len(merges)} merges, {differ[0]} merges missing, {differ[1]} not merges, "
           f"{differ[2]} in another group")
if any(differ):
    bad.append("step 3's pull requests are not the cohort's merges")
kept = rows(f"{K}/out/rework30_pairs.tsv")
by_case = {c: (sum(1 for r in kept if r[2] == c), sum(int(r[3]) for r in kept if r[2] == c))
           for c in ("same", "different", "unresolved")}
out.append(f"k5: rework30.py on step 3's keys keeps {len(kept)} pairs, "
           f"{sum(int(r[3]) for r in kept)} lines ("
           + ", ".join(f"{c} {p} pairs {l} lines" for c, (p, l) in by_case.items()) + ")")
out.append(f"replay.py git: {n} commits compared, {added} differ; {len(pairs)} pairs compared, "
           f"{deaths} differ | rework30.py --no-30d on 14_keys.py's keys: {figures} | k5: "
           f"{len(kept)} pairs kept, {sum(int(r[3]) for r in kept)} lines | as the archive's out/: "
           f"{', '.join(k for k, v in archive.items() if v) or 'none'}; differing: "
           f"{', '.join(k for k, v in archive.items() if not v) or 'none'} (lg/an/checks.txt)")
print("\n".join(out))
if bad:
    sys.exit("step 4: " + "; ".join(bad))
PY
  mv "$DIR/lg/an/checks.txt.part" "$DIR/lg/an/checks.txt"
  finish 4 "step 4: $(tail -n 1 "$DIR/lg/an/checks.txt")"
}

# missing_diffs: how many of ai/shas.txt's commits have no diff on disk
missing_diffs() {
  local sha n=0
  while read -r sha; do
    if [ -n "$sha" ] && [ ! -e "$DIR/ai/diffs/${sha:0:2}/$sha.diff.gz" ]; then
      n=$((n + 1))
    fi
  done < "$DIR/ai/shas.txt"
  echo "$n"
}

step5() {
  local shas="$DIR/ai/shas.txt" diffs="$DIR/ai/diffs" missing fetched=0
  mkdir -p "$diffs"
  if [ -e "$shas" ]; then
    cmp -s "$shas" "$FS/ai/shas.txt" || fail "step 5: $shas is not the archive's ai/shas.txt"
  else
    cp "$FS/ai/shas.txt" "$shas"
  fi
  missing="$(missing_diffs)"
  if [ "$missing" -gt 0 ]; then
    env -u GH_TOKEN PF_REPO="${AI_REPO:?set AI_REPO to the second repository, owner/name}" \
      python3 "$SS/scripts-feasibility/fetch_diff.py" "$shas" "$diffs" 4 "$diffs/stats.jsonl" \
      >> "$DIR/ai/fetch.log"
    fetched=$((missing - $(missing_diffs)))
    missing="$(missing_diffs)"
  fi
  [ "$missing" -eq 0 ] || fail "step 5: $missing diffs of ai/shas.txt are still missing (see $diffs/stats.jsonl)"
  finish 5 "step 5: second repository: $(grep -c . "$shas") of $(grep -c . "$shas") diffs of ai/shas.txt under $diffs, $fetched fetched now"
}

step1
step2
step3
step4
step5
