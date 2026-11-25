#!/usr/bin/env bash
# The changes job's filters, computed when it runs: `.github/scripts/check-ci-filters.sh generate`
# writes every filter of ci.yaml's changes job, computed from what each job reads (the files of
# .github/ci-gates, the //ci: lines of the Go tests, go list, and the tsc programs and bundles of
# .github/scripts/ci_ts_programs.py's LISTED trees). No file of the repository holds the filters,
# so two pull requests that each change what a job reads no longer edit one shared list.
#
# It writes two files. <out>.computed.yaml beside <out> is the filters as computed, which the lint
# job's readers (.github/scripts/check-ts-filters.sh, .github/scripts/check-image-trigger-paths.sh)
# hold to the tsc programs and the image builds. <out> is what the job's dorny/paths-filter step
# reads: the same filters, but each one the change may have altered without selecting it is `'**'`.
#
# THE COMPUTATION ITSELF. A change to a file the filters are computed with - the engine and the
# shared read table under .github/scripts, the scripts that run them, a file they read such as
# package.json (the generate mode lists them in <out>.computed.yaml.inputs) - can make many filters
# select less, while starting only the few jobs whose filters name that file: one dropped read in a
# helper every Bun gate shares would skip those jobs in the very run that drops it. So when
# CI_FILTERS_BASE names the commit the change is measured against (the pull request's base, the
# merge group's base, a dispatched branch's merge base with main) and the change touches one of
# those files, this computes the base tree's filters too, with the base tree's own engine, and
# every filter whose patterns differ between the two is `'**'` in <out>: the run selects every job
# whose selection the change could have moved, as the committed filters file used to, which every
# filter named. Each such filter is printed with the patterns only the base has and only this tree
# has, and so is the step summary.
#
# FAILING OPEN. When the computation fails for any reason - go or Bun missing, a go command that
# cannot run, a //ci: line or a gate file that cannot be read, a run longer than LIMIT, a crash -
# the filters are unknown, and a job a filter would have selected must not be skipped for it. So
# this writes instead, to both files, a file in which every filter the changes job hands on is
# `'**'`, which matches every changed path: every job runs, as on a dispatch of main. Its first line
# starts with `# FAIL-OPEN:` and says why, and the lint job's readers refuse such a file, so the
# run is red where the cause shows. A base tree whose filters cannot be computed, or a base commit
# that cannot be read, makes every filter `'**'` in <out> alone: the computed filters stand, and the
# lint job holds them. The names are every `steps.filter.outputs.<name>` .github/workflows/ci.yaml
# reads, which needs no engine to find and is the one form in which the changes job hands a job its
# filter (check-ci-filters.sh's rule 2), but `changes`: that is the action's own list of the filters
# that matched, which it writes only while no filter has the name (rule 19).
# A step reads its filter out of that list instead (the test job's harness steps), and the file
# holds no such filter, so either fallback also writes `every_filter=true` to the step's outputs
# (GITHUB_OUTPUT), which ci.yaml's matched_filters reads as `all`, as on a dispatch of main: every
# step gated on the list runs, whatever form its gate is written in.
# It exits 0 either way, so the changes job's outputs always come from its paths-filter step; it
# exits 1 only when it can write no file.
#
# Run from anywhere: .github/scripts/ci-filters-generate.sh <out>
# CI runs it in ci.yaml's changes job, writing "$RUNNER_TEMP/ci-filters.yaml".
set -uo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "::error file=.github/scripts/ci-filters-generate.sh::this takes the path to write the filters to" >&2
  exit 1
fi
out=$1
case "$out" in /*) ;; *) out="$PWD/$out" ;; esac
computed="${out%.yaml}.computed.yaml"
cd "$(dirname "$0")/../.." || exit 1
head_root=$PWD

# The generation took 46 s of the ci-filters job's check in run 44974321862 and 52 s of the changes
# job in run 45017125244: a run past 300 s is one that has stopped making progress. A child that
# ignores the TERM is killed 15 s later. The changes job's step timeout of 24 minutes holds this
# tree's run, the base's fetch and its retried install, and the base's run together; the comment
# above that step in ci.yaml adds them up, and the one above the job's timeout-minutes adds up
# the job.
LIMIT=${CI_FILTERS_LIMIT:-300}

# every_filter <first line>: a filters file whose every filter is `'**'`, under that first line,
# with the step's output every_filter=true, which hands every step gated on the list `all`.
every_filter() {
  local names
  names=$(grep -o 'steps\.filter\.outputs\.[A-Za-z0-9_-]*' .github/workflows/ci.yaml | sed 's/.*\.//' | grep -vx changes | sort -u)
  [ -n "$names" ] || return 1
  echo "$1"
  while IFS= read -r name; do
    printf '%s:\n  - %s\n' "$name" "'**'"
  done <<<"$names"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "every_filter=true" >>"$GITHUB_OUTPUT"
  fi
}

started=$(date +%s)
timeout -k 15 "$LIMIT" .github/scripts/check-ci-filters.sh generate "$computed"
status=$?
echo "the filter generation took $(($(date +%s) - started)) s and exited $status"
if [ "$status" -ne 0 ] || [ ! -s "$computed" ]; then
  why="check-ci-filters.sh generate exited $status"
  [ "$status" -eq 124 ] && why="check-ci-filters.sh generate ran past ${LIMIT} s"
  if ! every_filter "# FAIL-OPEN: the filters could not be computed ($why), so every filter matches every path and every job runs." >"$out"; then
    echo "::error file=.github/workflows/ci.yaml::the filters could not be computed ($why), and no output of ci.yaml reads steps.filter.outputs.<name>, so no file that selects every job can be written either" >&2
    exit 1
  fi
  cp "$out" "$computed" || exit 1
  echo "::error file=.github/scripts/ci-filters-generate.sh::the filters could not be computed ($why), so every filter of the changes job matches every path and every job runs; the lines above say why"
  exit 0
fi
cp "$computed" "$out" || exit 1
# Two runs over one tree compute one text, which this digest lets a reader of the logs compare.
echo "the computed filters' sha256: $(sha256sum <"$computed" | cut -d' ' -f1)"

# force_every_filter <why>: <out> with every filter `'**'`, the computed filters left as they are.
force_every_filter() {
  every_filter "# FORCED: every filter matches every path: $1." >"$out" || cp "$computed" "$out"
  echo "::warning file=.github/scripts/ci-filters-generate.sh::$1; every filter matches every path and every job runs"
}

# The base tree's work gets what is left of LIMIT after this tree's, so the two together stay
# within it, and the fetch and the install are bounded as well.
left() { echo $((LIMIT - ($(date +%s) - started))); }

base=${CI_FILTERS_BASE:-}
[ -n "$base" ] || exit 0
if ! git cat-file -e "$base^{commit}" 2>/dev/null; then
  timeout -k 5 60 git fetch -q --no-tags --depth=1 origin "$base" 2>&1 | tail -3
fi
# --no-renames: a renamed file is its old path deleted and its new one added, so a tsconfig moved
# away is counted as touched by its old name too.
if ! changed=$(git diff --no-renames --name-only "$base" HEAD 2>/dev/null); then
  force_every_filter "the base commit $base cannot be read, so whether this change alters the computation is unknown"
  exit 0
fi
# A file this tree's run read is one input; a resolution file anywhere is another, since adding or
# deleting a nearer tsconfig or package.json moves what tsc and bun resolve with though this tree's
# run never opened it.
touched=$( (comm -12 <(sort -u <<<"$changed") <(sort -u "$computed.inputs"); \
  grep -E '(^|/)(tsconfig[^/]*\.json|package\.json|bunfig\.toml)$' <<<"$changed") | sort -u)
if [ -z "$touched" ]; then
  echo "this change touches no file the filters are computed with; the filters stand as computed"
  exit 0
fi
echo "this change touches $(wc -l <<<"$touched") file(s) the filters are computed with:"
sed 's/^/  /' <<<"$touched"

scratch=$(mktemp -d)
trap 'git -C "$head_root" worktree remove --force "$scratch/base" >/dev/null 2>&1; rm -rf "$scratch"' EXIT
base_started=$(date +%s)
if ! git worktree add -q --detach "$scratch/base" "$base" 2>/dev/null; then
  force_every_filter "the base tree $base cannot be checked out, so the filters it computes are unknown"
  exit 0
fi
# The base tree's listing needs its own node_modules, which a frozen install fills from the cache
# this tree's install just warmed. It retries as every Bun install of this repository's CI does
# (.github/ci-budgets.md's Retries note): five attempts of 180 s each, 5 x 180 + 150 = 1050 s at
# the worst, which the changes job's step budget holds. An install that takes what is left of
# LIMIT leaves the base's run no time, and the check below makes every filter `'**'` then.
if [ -f "$scratch/base/package.json" ] && command -v bun >/dev/null; then
  # shellcheck source=.github/scripts/ci-retry.sh
  . .github/scripts/ci-retry.sh
  (cd "$scratch/base" && retry_fetch "the base tree's dependencies" 180 -- bun install --frozen-lockfile >/dev/null 2>&1)
fi
if [ "$(left)" -le 15 ]; then
  force_every_filter "no time is left of the ${LIMIT} s for the base tree's filters, so which filters this change alters is unknown"
  exit 0
fi
timeout -k 15 "$(left)" "$scratch/base/.github/scripts/check-ci-filters.sh" generate "$scratch/base.yaml" >"$scratch/base.log" 2>&1
base_status=$?
echo "the base tree's filter generation took $(($(date +%s) - base_started)) s and exited $base_status"
if [ "$base_status" -ne 0 ] || [ ! -s "$scratch/base.yaml" ]; then
  echo "the base tree's generation printed, last lines:"
  # The base tree's own output, its workflow commands kept from acting: an ::error:: of the base
  # would otherwise annotate this tree's files on a job that passes.
  token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  echo "::stop-commands::$token"
  tail -20 "$scratch/base.log" | sed 's/^/  /'
  echo "::$token::"
  force_every_filter "the base tree's filters could not be computed (its check-ci-filters.sh generate exited $base_status), so which filters this change alters is unknown"
  exit 0
fi

python3 - "$computed" "$scratch/base.yaml" "$out" "${GITHUB_STEP_SUMMARY:-}" <<'PY' || force_every_filter "the computed filters could not be compared with the base tree's"
import sys

import yaml

computed_path, base_path, out_path, summary_path = sys.argv[1:5]


def flatten(value):
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [p for item in value for p in flatten(item)]
    return []


head = {str(k): flatten(v) for k, v in (yaml.safe_load(open(computed_path)) or {}).items()}
base = {str(k): flatten(v) for k, v in (yaml.safe_load(open(base_path)) or {}).items()}
changed = sorted(name for name in head if head[name] != base.get(name))
only_base = sorted(name for name in base if name not in head)
lines = []
for name in changed:
    if name not in base:
        lines.append(f"{name}: no such filter in the base tree")
        continue
    gone = [p for p in base[name] if p not in head[name]]
    new = [p for p in head[name] if p not in base[name]]
    lines.append(f"{name}: {len(gone)} pattern(s) only in the base tree, {len(new)} only in this one")
    lines += [f"  - {p}" for p in gone[:20]] + ([f"  - and {len(gone) - 20} more"] if len(gone) > 20 else [])
    lines += [f"  + {p}" for p in new[:20]] + ([f"  + and {len(new) - 20} more"] if len(new) > 20 else [])
lines += [f"{name}: a filter of the base tree only, which no gate of this tree declares" for name in only_base]
text = "\n".join(lines)
print(f"{len(changed)} of {len(head)} filters differ from the base tree's; each selects every path in this run")
if text:
    print(text)
if summary_path and changed:
    with open(summary_path, "a") as summary:
        summary.write(f"### {len(changed)} filter(s) this change alters, selecting every path in this run\n\n```\n{text}\n```\n")
if changed:
    out = [f"# FORCED: {', '.join(changed)} differ from the base tree's filters, so each matches every path."]
    for name in sorted(head):
        out.append(f"{name}:")
        out += ["  - '**'"] if name in changed else [f"  - '{p.replace(chr(39), chr(39) * 2)}'" for p in head[name]]
    open(out_path, "w").write("\n".join(out) + "\n")
PY
exit 0
