#!/usr/bin/env bash
# scored-job.sh: one build-cluster Job of a scored scenario, run at the repository's root of a
# runner's pod by `legion-remote-test.sh --sh`. It runs the rig (rig.sh) on its scenario's label
# group and prints, in the Job's log, each run's row, a `reach` line for each entry of each run's
# reach, the last lines of each log of a run the rig failed, and a line for each case's cell
# (rig.sh cell), the last line the last case's.
#
#   scored-job.sh <scenario> <label group> [--case <case>] [--count <n>]
#
# Its one scenario is owner-walk. Its label groups, each on the commit of the label
# checkout (SKILL_SCENARIOS_LABEL_CHECKOUT, default this checkout). A control group's label tree
# is a worktree of THIS checkout at that commit (rig.sh label-tree), so a label checkout must be
# this checkout or a worktree of it: one whose objects this checkout does not hold runs the `head`
# group and stops `controls` and `strict` with "<commit> not in this checkout".
#   head      the label `head`, the checkout itself: forms A, B and C
#   controls  ctl-d, the tree of controls/owner-walk-D.txt, and ctl-e, of controls/owner-walk-E.txt
#             (rig.sh label-tree): forms D and E. ctl-d's tree must differ from its commit in the
#             lines of D's patch alone (rig.sh unpatch-holds), or the Job stops before any run.
#             ctl-e is the control of form E only at a label that carries #5564's skills trim
#             (controls/owner-walk-E.txt)
#   strict    ctl-d-strict, the tree of controls/owner-walk-D-strict.txt: form D
# A Job builds the trees and profiles of its cases' labels alone: a Job of form E's cases builds no
# ctl-d, so it runs at a label checkout where D's patch does not apply.
# --case <kind>-<form>-<family> runs one case, of a form of the group; without it, every case of
# the group's forms of the family SKILL_SCENARIOS_FAMILY names (claude or gpt). --count is each
# case's scored runs, by default its form's runs (owner-walk.ts cases). Each case runs as rig.sh
# batch-scored's cell: runs until that many score, at most three times as many launches, one at a
# time. A case of form A, B or C ends on its case's cell; one of a control form, D or E, on the
# control's own cell over every owner of the form, which one case's runs cannot fill: the Job
# prints it UNKNOWN until each owner's runs are in.
#
# In order: `bun install --frozen-lockfile`; the pod's own Postgres and NATS (LEGION_TEST_PG_DSN,
# LEGION_TEST_NATS_URL) as the rig's SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL, so
# the batch starts no container, and its Oh My Pi (LEGION_TEST_OMP) as SKILL_SCENARIOS_OMP, each
# unless the variable of the rig's own is set; the capture of a real plans read at the label
# checkout (rig.sh plans-capture), whose answer every run's proxy serves, its keys mapped to the
# run's; each label's tree, a control tree's `bun install --frozen-lockfile`, and its profile; each
# case's batch; then the rows, the reach lines and the cells.
#
# The model: SKILL_SCENARIOS_MODEL=standin runs every turn on the model stand-in (model-standin.ts).
# Without it, the gateway, on the family's block of a provider file the Job writes: `gateway`
# (anthropic-messages, at the gateway's root) for claude, `gateway-openai` (openai-responses, at the
# root's /v1) for gpt, each reading its key with the key command. Its inputs, refused unset:
# SKILL_SCENARIOS_GATEWAY_URL (the gateway's root) and SKILL_SCENARIOS_MODEL_ID (the model the
# family's runs take), and SKILL_SCENARIOS_MODEL_KEY_COMMAND (default /work/bin/model-token, which
# the runner's --model-gateway writes). Forms A, C and D take SKILL_SCENARIOS_GUIDANCE_MODELS too
# (rig.sh's header).
#
# The reach lines record a run's reach and stop none: the reach is a measurement and no guard. Only a
# plain-text mention of one of the three hosts in a tool's arguments reaches it, and a host reached
# through a variable, an encoding or a file is not listed.
#
# The Job keeps no run after the pod: it prints each run's row in its log, and stores none.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
root=$(cd "$here/../../../.." && pwd -P)
rig=$here/rig.sh
usage="usage: scored-job.sh <scenario> <label group> [--case <case>] [--count <n>]"

say() { printf 'scored-job: %s\n' "$*"; }
refuse() {
  echo "scored-job: $*" >&2
  exit 2
}

[ $# -ge 2 ] || refuse "$usage"
scenario=$1 group=$2
shift 2
one_case='' count=''
while [ $# -gt 0 ]; do
  case $1 in
  --case | --count)
    [ $# -ge 2 ] || refuse "$1 needs a value; $usage"
    if [ "$1" = --case ]; then one_case=$2; else count=$2; fi
    shift 2
    ;;
  *) refuse "$1 is no option; $usage" ;;
  esac
done
[ "$scenario" = owner-walk ] ||
  refuse "$scenario is no scenario a Job runs: owner-walk is the one"
[ -z "$count" ] || [[ $count =~ ^[1-9][0-9]*$ ]] || refuse "--count '$count' is not a whole number above 0"
case $group in
head) labels=(head) forms=(A B C) ;;
controls) labels=(ctl-d ctl-e) forms=(D E) ;;
strict) labels=(ctl-d-strict) forms=(D) ;;
*) refuse "the label group '$group' is none of head, controls and strict" ;;
esac

# label_of FORM is the group's label of FORM; manifest_of LABEL the manifest of a control label.
label_of() {
  case $group:$1 in
  head:*) echo head ;;
  controls:D) echo ctl-d ;;
  controls:E) echo ctl-e ;;
  strict:D) echo ctl-d-strict ;;
  esac
}
manifest_of() {
  case $1 in
  ctl-d) echo "$here/controls/owner-walk-D.txt" ;;
  ctl-e) echo "$here/controls/owner-walk-E.txt" ;;
  ctl-d-strict) echo "$here/controls/owner-walk-D-strict.txt" ;;
  esac
}

# The cases of the Job, each with its count.
all_cases=$(bun "$here/owner-walk.ts" cases) || refuse "owner-walk.ts cases failed"
selected=()
if [ -n "$one_case" ]; then
  grep -q "^$one_case " <<<"$all_cases" ||
    refuse "--case '$one_case' is no case of owner-walk; its cases are $(cut -d' ' -f1 <<<"$all_cases" | paste -sd' ')"
  IFS=- read -r _ form _ <<<"$one_case"
  [[ " ${forms[*]} " == *" $form "* ]] || refuse "--case $one_case is of form $form, which the label group $group does not run (${forms[*]})"
  selected=("$one_case")
else
  case ${SKILL_SCENARIOS_FAMILY:-} in claude | gpt) ;; *) refuse "a Job of every case of its group runs one family: set SKILL_SCENARIOS_FAMILY to claude or gpt, or name --case" ;; esac
  while read -r name _; do
    IFS=- read -r _ form family <<<"$name"
    [ "$family" = "$SKILL_SCENARIOS_FAMILY" ] && [[ " ${forms[*]} " == *" $form "* ]] || continue
    selected+=("$name")
  done <<<"$all_cases"
fi
[ ${#selected[@]} -gt 0 ] || refuse "owner-walk.ts cases names no case of the forms ${forms[*]}"
family=${selected[0]##*-}
case_count() { if [ -n "$count" ]; then echo "$count"; else sed -n "s/^$1 //p" <<<"$all_cases"; fi; }

# The pod's own services and Oh My Pi, as the rig's inputs.
export SKILL_SCENARIOS_WORK=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/scored-job}
if [ -z "${SKILL_SCENARIOS_POSTGRES_URL:-}${SKILL_SCENARIOS_NATS_URL:-}" ] && [ -n "${LEGION_TEST_PG_DSN:-}" ] && [ -n "${LEGION_TEST_NATS_URL:-}" ]; then
  export SKILL_SCENARIOS_POSTGRES_URL=$LEGION_TEST_PG_DSN SKILL_SCENARIOS_NATS_URL=$LEGION_TEST_NATS_URL
fi
if [ -z "${SKILL_SCENARIOS_OMP:-}" ] && [ -n "${LEGION_TEST_OMP:-}" ]; then export SKILL_SCENARIOS_OMP=$LEGION_TEST_OMP; fi
mkdir -p "$SKILL_SCENARIOS_WORK"

# The model route.
if [ "${SKILL_SCENARIOS_MODEL:-}" = standin ]; then
  say "the model: the model stand-in (model-standin.ts)"
else
  [ -z "${SKILL_SCENARIOS_MODEL:-}" ] || refuse "SKILL_SCENARIOS_MODEL is '$SKILL_SCENARIOS_MODEL': it is standin, or unset for the gateway"
  [ -n "${SKILL_SCENARIOS_GATEWAY_URL:-}" ] || refuse "SKILL_SCENARIOS_GATEWAY_URL is unset: name the model gateway's root, or set SKILL_SCENARIOS_MODEL=standin"
  [ -n "${SKILL_SCENARIOS_MODEL_ID:-}" ] || refuse "SKILL_SCENARIOS_MODEL_ID is unset: name the model of the $family family's runs"
  provider=gateway api=anthropic-messages base=${SKILL_SCENARIOS_GATEWAY_URL%/}
  [ "$family" = claude ] || provider=gateway-openai api=openai-responses base=$base/v1
  jq -n --arg provider "$provider" --arg api "$api" --arg base "$base" --arg model "$SKILL_SCENARIOS_MODEL_ID" \
    --arg key "!${SKILL_SCENARIOS_MODEL_KEY_COMMAND:-/work/bin/model-token}" \
    '{providers: {($provider): {baseUrl: $base, api: $api, apiKey: $key, authHeader: true, models: [{id: $model}]}}}' \
    >"$SKILL_SCENARIOS_WORK/providers.yml"
  export LEGION_E2E_MODEL_ROUTE=provider LEGION_E2E_MODEL_PROVIDER=$provider LEGION_E2E_MODEL_PROVIDER_MODEL=$SKILL_SCENARIOS_MODEL_ID
  export LEGION_E2E_MODEL_PROVIDER_FILE=$SKILL_SCENARIOS_WORK/providers.yml
  say "the model: $provider/$SKILL_SCENARIOS_MODEL_ID"
fi

checkout=$(cd "${SKILL_SCENARIOS_LABEL_CHECKOUT:-$root}" && pwd -P)
commit=$(git -C "$checkout" rev-parse HEAD)
say "$scenario, label group $group, cases ${selected[*]}, label checkout $checkout at $commit, work $SKILL_SCENARIOS_WORK"
say "bun install --frozen-lockfile"
(cd "$root" && bun install --frozen-lockfile) >"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
  refuse "bun install --frozen-lockfile failed; see $SKILL_SCENARIOS_WORK/bun-install.log"
[ "$checkout" = "$root" ] || (cd "$checkout" && bun install --frozen-lockfile) >>"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
  refuse "bun install --frozen-lockfile in $checkout failed; see $SKILL_SCENARIOS_WORK/bun-install.log"

# The recorded plans read, at the label checkout.
bash "$rig" plans-capture "$checkout" "$SKILL_SCENARIOS_WORK/plans"
export SKILL_SCENARIOS_PLANS_ANSWER=$SKILL_SCENARIOS_WORK/plans

# Each label of the Job's cases, in the group's order: its tree (a control's), and its profile.
# The profile packs the label's plugins from that tree, and a pack builds through the tree's own
# node_modules (scripts/e2e/lib/pack-plugin.sh), which a new control tree has none of.
used=()
for label in "${labels[@]}"; do
  for name in "${selected[@]}"; do
    IFS=- read -r _ form _ <<<"$name"
    if [ "$(label_of "$form")" = "$label" ]; then
      used+=("$label")
      break
    fi
  done
done
for label in "${used[@]}"; do
  tree=$checkout
  if [ "$label" != head ]; then
    bash "$rig" label-tree "$label" "$commit" "$(manifest_of "$label")"
    tree=$SKILL_SCENARIOS_WORK/label-trees/$label/tree
    [ "$label" != ctl-d ] || bash "$rig" unpatch-holds ctl-d "$here/controls/patches/owner-walk-D.patch"
    (cd "$tree" && bun install --frozen-lockfile) >>"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
      refuse "bun install --frozen-lockfile in the label tree $label failed; see $SKILL_SCENARIOS_WORK/bun-install.log"
  fi
  bash "$rig" profile "$label" "$tree"
done

# Each case's batch, then the rows and the reach lines, then the cells.
for name in "${selected[@]}"; do
  IFS=- read -r _ form _ <<<"$name"
  SKILL_SCENARIOS_OWNER_WALK_CASE=$name bash "$rig" batch-scored "$scenario" "$(case_count "$name")" "$(label_of "$form")"
done
bash "$rig" rows "$scenario"
# A run the rig failed keeps its logs in the pod alone, and the pod keeps nothing: the last lines of
# each, so the Job's log says why.
for row in "$SKILL_SCENARIOS_WORK/runs/$scenario"-*/row.json; do
  [ -f "$row" ] && [ "$(jq -r .outcome "$row")" = rig-error ] || continue
  dir=$(dirname -- "$row")
  for log in "$dir"/logs/*.log "$dir"/daemon.log "$dir"/fixture.log "$dir"/texts/label-texts.log "$dir"/omp-stderr.txt \
    "$SKILL_SCENARIOS_WORK/scored-logs/${dir##*/}.err"; do
    [ -s "$log" ] || continue
    say "rig error ${dir##*/}: the last lines of ${log#"$SKILL_SCENARIOS_WORK"/}"
    tail -n 20 "$log" | sed 's/^/  /'
  done
done
[ -n "$one_case" ] || bun "$here/score.ts" cells "$SKILL_SCENARIOS_WORK/runs" "$scenario"
for name in "${selected[@]}"; do
  IFS=- read -r _ form _ <<<"$name"
  cell=$(bash "$rig" cell "$scenario" "$(label_of "$form")" "$name" "$(case_count "$name")")
  printf 'scored-job %s %s %s, label %s: %s\n' "$scenario" "$group" "$name" "$(label_of "$form")" "${cell#*: }"
done
