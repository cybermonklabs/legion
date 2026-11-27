#!/usr/bin/env bash
# scored-job.sh: one build-cluster Job of a scored scenario, run at the repository's root of a
# runner's pod by `legion-remote-test.sh --sh`. It runs the rig (rig.sh) on its scenario's label
# group and prints, in the Job's log, each run's row, a `reach` line for each entry of each run's
# reach, the last lines of each log of a run the rig failed, and a line for each case's cell
# (rig.sh cell), the last line the last case's.
#
#   scored-job.sh owner-walk <label group> [--case <case>] [--count <n>]
#   scored-job.sh <forward-only|forward-owner> <label> [--world <world>] [--count <n>]
#                 [--path-check [--path-control intercept]]
#
# owner-walk's label groups, each on the commit of the label checkout
# (SKILL_SCENARIOS_LABEL_CHECKOUT, default this checkout). A control group's label tree is a
# worktree of THIS checkout at that commit (rig.sh label-tree), so a label checkout must be this
# checkout or a worktree of it: one whose objects this checkout does not hold runs the `head` group
# and stops `controls` and `strict` with "<commit> not in this checkout".
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
# A forward scenario's Job runs one label, on the commit of the label checkout, over each world of
# the label (forward_worlds), or the one --world names, each world a cell of --count scored runs
# (5 by default): the label `head`, the checkout itself, every world of its scenario; or a control
# label, its tree of its manifest under controls/ with its patch under controls/patches/ taken out
# (rig.sh label-tree): ctl-main-texts and ctl-forward-only (retro-defect, merge-defect),
# ctl-thread (retro-thread), ctl-owner (forward-owner's three worlds). A control label's tree must
# differ from its commit in its patch's lines alone (rig.sh unpatch-holds), or the Job prints C6's
# first row, `UNKNOWN: <patch> is not the pull request's diff at <commit>`, starts no run, and exits
# 3. Each world's cell ends on its lines of `rig.sh forward-cell`: each discarded launch, C6's
# decision of a control label's cell (or the head's line), and whether the head's Job of the world
# may start. --path-check (SKILL_SCENARIOS_MODEL=standin-report, label head) builds the label with
# `label-texts.sh build --compile` and prints, for each run, the path check's one line
# (`score.ts path-check`); --path-control intercept gives each run's pane the intercepting proxy
# (paneDaemon intercept-report), the path check's control.
#
# In order: the pod's own Postgres and NATS (LEGION_TEST_PG_DSN, LEGION_TEST_NATS_URL) as the rig's
# SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL, so the batch starts no container, and
# its Oh My Pi (LEGION_TEST_OMP) as SKILL_SCENARIOS_OMP, each unless the variable of the rig's own
# is set; the model route; `bun install --frozen-lockfile`; for owner-walk, the capture of a real
# plans read at the label checkout (rig.sh plans-capture), whose answer every run's proxy serves,
# its keys mapped to the run's; each label's tree, a control tree's `bun install --frozen-lockfile`,
# and its profile; each case's or world's batch; then the rows, the reach lines and the cells.
#
# The model: SKILL_SCENARIOS_MODEL=standin runs every turn on the model stand-in (model-standin.ts),
# and standin-report on its scripted mode, a path check's. Without it, the gateway, on the family's
# block of a provider file the Job writes: `gateway` (anthropic-messages, at the gateway's root) for
# claude, `gateway-openai` (openai-responses, at the root's /v1) for gpt, each reading its key with
# the key command. Its inputs, refused unset: SKILL_SCENARIOS_GATEWAY_URL (the gateway's root) and
# SKILL_SCENARIOS_MODEL_ID (the model the family's runs take; a forward Job's is claude-opus-5-5,
# its default, and it refuses another), and SKILL_SCENARIOS_MODEL_KEY_COMMAND (default
# /work/bin/model-token, which the runner's --model-gateway writes). Forms A, C and D take
# SKILL_SCENARIOS_GUIDANCE_MODELS too (rig.sh's header).
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
usage="usage: scored-job.sh owner-walk <label group> [--case <case>] [--count <n>] | scored-job.sh <forward-only|forward-owner> <label> [--world <world>] [--count <n>] [--path-check [--path-control intercept]]"
# The one model of a forward scenario's runs (C5 of dispatch://INFRA-5581's plan).
forward_model=claude-opus-5-5

say() { printf 'scored-job: %s\n' "$*"; }
refuse() {
  echo "scored-job: $*" >&2
  exit 2
}

[ $# -ge 2 ] || refuse "$usage"
scenario=$1 group=$2
shift 2
one_case='' count='' one_world='' path_check='' path_control=''
while [ $# -gt 0 ]; do
  case $1 in
  --case | --count | --world | --path-control)
    [ $# -ge 2 ] || refuse "$1 needs a value; $usage"
    case $1 in
    --case) one_case=$2 ;;
    --count) count=$2 ;;
    --world) one_world=$2 ;;
    --path-control) path_control=$2 ;;
    esac
    shift 2
    ;;
  --path-check)
    path_check=1
    shift
    ;;
  *) refuse "$1 is no option; $usage" ;;
  esac
done
[ -z "$count" ] || [[ $count =~ ^[1-9][0-9]*$ ]] || refuse "--count '$count' is not a whole number above 0"

# The pod's own services and Oh My Pi, as the rig's inputs.
pod_services() {
  export SKILL_SCENARIOS_WORK=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/scored-job}
  if [ -z "${SKILL_SCENARIOS_POSTGRES_URL:-}${SKILL_SCENARIOS_NATS_URL:-}" ] && [ -n "${LEGION_TEST_PG_DSN:-}" ] && [ -n "${LEGION_TEST_NATS_URL:-}" ]; then
    export SKILL_SCENARIOS_POSTGRES_URL=$LEGION_TEST_PG_DSN SKILL_SCENARIOS_NATS_URL=$LEGION_TEST_NATS_URL
  fi
  if [ -z "${SKILL_SCENARIOS_OMP:-}" ] && [ -n "${LEGION_TEST_OMP:-}" ]; then export SKILL_SCENARIOS_OMP=$LEGION_TEST_OMP; fi
  mkdir -p "$SKILL_SCENARIOS_WORK"
}

# model_route FAMILY: the model stand-in, or the gateway's block of FAMILY (the header).
model_route() {
  local family=$1 provider api base
  case ${SKILL_SCENARIOS_MODEL:-} in
  standin)
    say "the model: the model stand-in (model-standin.ts)"
    return
    ;;
  standin-report)
    say "the model: the model stand-in (model-standin.ts), playing each run's standin-script.json"
    return
    ;;
  '') ;;
  *) refuse "SKILL_SCENARIOS_MODEL is '$SKILL_SCENARIOS_MODEL': it is standin or standin-report, or unset for the gateway" ;;
  esac
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
}

# bun_install runs `bun install --frozen-lockfile` in this checkout and in the label checkout.
bun_install() {
  say "bun install --frozen-lockfile"
  (cd "$root" && bun install --frozen-lockfile) >"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
    refuse "bun install --frozen-lockfile failed; see $SKILL_SCENARIOS_WORK/bun-install.log"
  [ "$checkout" = "$root" ] || (cd "$checkout" && bun install --frozen-lockfile) >>"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
    refuse "bun install --frozen-lockfile in $checkout failed; see $SKILL_SCENARIOS_WORK/bun-install.log"
}

# rig_error_tails SCENARIO: a run the rig failed keeps its logs in the pod alone, and the pod keeps
# nothing: the last lines of each, so the Job's log says why.
rig_error_tails() {
  local row dir log
  for row in "$SKILL_SCENARIOS_WORK/runs/$1"-*/row.json; do
    [ -f "$row" ] && [ "$(jq -r .outcome "$row")" = rig-error ] || continue
    dir=$(dirname -- "$row")
    for log in "$dir"/logs/*.log "$dir"/daemon.log "$dir"/fixture.log "$dir"/texts/label-texts.log "$dir"/omp-stderr.txt \
      "$SKILL_SCENARIOS_WORK/scored-logs/${dir##*/}.err"; do
      [ -s "$log" ] || continue
      say "rig error ${dir##*/}: the last lines of ${log#"$SKILL_SCENARIOS_WORK"/}"
      tail -n 20 "$log" | sed 's/^/  /'
    done
  done
}

# ---- forward-only and forward-owner ----

# forward_worlds LABEL prints the worlds of LABEL's cells, one a line, and fails for a label the
# scenario has not.
forward_worlds() {
  case $scenario:$1 in
  forward-only:head) printf '%s\n' retro-defect merge-defect merge-refused retro-thread ;;
  forward-owner:head | forward-owner:ctl-owner) printf '%s\n' defect-notice child-held request-after-signoff ;;
  forward-only:ctl-main-texts | forward-only:ctl-forward-only) printf '%s\n' retro-defect merge-defect ;;
  forward-only:ctl-thread) printf '%s\n' retro-thread ;;
  *) return 1 ;;
  esac
}

# forward_control LABEL prints a control label's manifest and patch, relative to this directory.
forward_control() {
  case $1 in
  ctl-main-texts) printf '%s\n' controls/forward-only-main-texts.txt controls/patches/forward-only-texts.patch ;;
  ctl-forward-only) printf '%s\n' controls/forward-only-sentences.txt controls/patches/forward-only-sentences.patch ;;
  ctl-thread) printf '%s\n' controls/forward-only-thread.txt controls/patches/forward-only-thread.patch ;;
  ctl-owner) printf '%s\n' controls/forward-owner.txt controls/patches/forward-owner.patch ;;
  esac
}

# The texts that the implementer in retro or the merger reads, which ctl-main-texts takes out
# (C6). Every other control label takes out one commit of the texts pull request.
forward_text_paths=(
  packages/daemon/internal/prompts/roles/core/common.md
  packages/daemon/internal/prompts/roles/core/implementer.md
  packages/daemon/internal/prompts/roles/implementer.md
  packages/daemon/internal/prompts/go/worker-common.md
  packages/daemon/internal/prompts/roles/merger.md
  skills/legion-retro
  skills/legion-worker
)

# forward_patch_group LABEL prints what the label's patch is the diff of: the subject of the one
# commit of the texts pull request, and `texts` for the diff of every text of the list above. A
# subject names its commit, so the patch is made again after a rebase gives it a new id.
forward_patch_group() {
  case $1 in
  ctl-main-texts) printf 'texts\n' ;;
  ctl-forward-only) printf '%s\n' 'docs(prompts): the retro and the merger report a defect after the last step and go on' ;;
  ctl-thread) printf '%s\n' 'docs(prompts): at the retro, a review thread that needs a fix is reported, not sent back' ;;
  ctl-owner) printf '%s\n' 'docs(prompts): the architect asks no role to move reviewed work back past the last step' ;;
  esac
}

# forward_commit_of SUBJECT prints the one commit of the label checkout's history whose subject is
# SUBJECT. None, or more than one, is no commit to make a patch from, and it fails.
forward_commit_of() {
  local found count
  found=$(git -C "$checkout" log --format='%H%x09%s' -n 400 HEAD | awk -F'\t' -v want="$1" '$2 == want { print $1 }')
  count=$(printf '%s' "$found" | grep -c . || true)
  [ "$count" = 1 ] || {
    say "the history holds $count commits whose subject is '$1', and a patch is made from one"
    return 1
  }
  printf '%s\n' "$found"
}

# forward_patch_made LABEL writes the patch the pull request's own commits make at this head. Each
# flag is given rather than read from the configuration, so the patch a dev box committed and the
# patch a pod makes again are the same bytes.
forward_patch_made() {
  local group commit first
  group=$(forward_patch_group "$1")
  if [ "$group" != texts ]; then
    commit=$(forward_commit_of "$group") || return 1
    git -C "$checkout" diff --no-color --no-ext-diff --no-renames --unified=3 --diff-algorithm=myers \
      --src-prefix=a/ --dst-prefix=b/ "$commit^" "$commit"
    return
  fi
  # The texts patch is the pull request's own diff of those files: from the parent of its first
  # commit to the head. A merge of the base into the texts branch brings other changes of those
  # files into that range, so the patch stops holding and this check says so.
  first=$(forward_commit_of "$(forward_patch_group ctl-forward-only)") || return 1
  git -C "$checkout" diff --no-color --no-ext-diff --no-renames --unified=3 --diff-algorithm=myers \
    --src-prefix=a/ --dst-prefix=b/ "$first^" HEAD -- "${forward_text_paths[@]}"
}

# forward_patch_holds LABEL PATCH: the committed patch is the diff that the pull request's own
# commits make at this head (C6, row 1). The manifest's own checks do not hold this. `label-tree`
# asks only that the patch applies in reverse, and `unpatch-holds` asks only that the tree differs
# from its commit in the patch's own lines. A patch one file short of the change, or one file
# longer, passes both, and its label's cells then measure another change.
forward_patch_holds() {
  local made=$SKILL_SCENARIOS_WORK/$1-made.patch
  if ! forward_patch_made "$1" >"$made" 2>"$made.err"; then
    say "the patch of $1 could not be made again: $(cat "$made.err")"
    return 1
  fi
  cmp -s "$made" "$2" && return 0
  say "the committed patch of $1 is not the diff its own commits make; that diff is $made"
  return 1
}

forward_job() {
  local label=$group worlds=() world control=() tree dir
  local -a batch_env=()
  mapfile -t worlds < <(forward_worlds "$label" || true)
  [ ${#worlds[@]} -gt 0 ] ||
    refuse "$label is no label of $scenario: forward-only takes head, ctl-main-texts, ctl-forward-only and ctl-thread; forward-owner takes head and ctl-owner"
  if [ -n "$one_world" ]; then
    printf '%s\n' "${worlds[@]}" | grep -qxF -- "$one_world" ||
      refuse "--world '$one_world' is no world of the label $label of $scenario: its worlds are ${worlds[*]}"
    worlds=("$one_world")
  fi
  count=${count:-5}
  if [ -n "$path_check" ]; then
    [ "${SKILL_SCENARIOS_MODEL:-}" = standin-report ] ||
      refuse "--path-check plays the scripted model: set SKILL_SCENARIOS_MODEL=standin-report"
    [ "$label" = head ] || refuse "--path-check runs at the label head, not $label"
    batch_env+=(SKILL_SCENARIOS_PATH_CHECK=1)
  fi
  case $path_control in
  '') ;;
  intercept)
    [ -n "$path_check" ] || refuse "--path-control intercept is a control of the path check: give --path-check too"
    batch_env+=(SKILL_SCENARIOS_PATH_CONTROL=intercept)
    ;;
  *) refuse "--path-control '$path_control' is no control: it is intercept" ;;
  esac
  [ -n "${SKILL_SCENARIOS_MODEL:-}" ] || [ "${SKILL_SCENARIOS_MODEL_ID:=$forward_model}" = "$forward_model" ] ||
    refuse "SKILL_SCENARIOS_MODEL_ID is '$SKILL_SCENARIOS_MODEL_ID': a $scenario run takes $forward_model alone"
  export SKILL_SCENARIOS_MODEL_ID
  pod_services
  model_route claude
  checkout=$(cd "${SKILL_SCENARIOS_LABEL_CHECKOUT:-$root}" && pwd -P)
  commit=$(git -C "$checkout" rev-parse HEAD)
  say "$scenario, label $label, worlds ${worlds[*]}, $count scored runs each, label checkout $checkout at $commit, work $SKILL_SCENARIOS_WORK"
  bun_install
  tree=$checkout
  if [ "$label" != head ]; then
    mapfile -t control < <(forward_control "$label")
    if ! forward_patch_holds "$label" "$here/${control[1]}" ||
      ! bash "$rig" label-tree "$label" "$commit" "$here/${control[0]}" ||
      ! bash "$rig" unpatch-holds "$label" "$here/${control[1]}"; then
      echo "UNKNOWN: ${control[1]} is not the pull request's diff at $commit"
      exit 3
    fi
    tree=$SKILL_SCENARIOS_WORK/label-trees/$label/tree
    (cd "$tree" && bun install --frozen-lockfile) >>"$SKILL_SCENARIOS_WORK/bun-install.log" 2>&1 ||
      refuse "bun install --frozen-lockfile in the label tree $label failed; see $SKILL_SCENARIOS_WORK/bun-install.log"
  fi
  bash "$rig" profile "$label" "$tree"
  for world in "${worlds[@]}"; do
    env "${batch_env[@]}" SKILL_SCENARIOS_FORWARD_WORLD="$world" bash "$rig" batch-scored "$scenario" "$count" "$label"
  done
  bash "$rig" rows "$scenario"
  rig_error_tails "$scenario"
  if [ -n "$path_check" ]; then
    for world in "${worlds[@]}"; do
      for dir in "$SKILL_SCENARIOS_WORK/runs/$scenario-$world-$label-"*/; do
        [ -d "$dir" ] || continue
        dir=${dir%/}
        # A launch the rig failed never reached the label's daemon, so row 1's line would blame it.
        if jq -e '.outcome == "rig-error"' "$dir/row.json" >/dev/null 2>&1; then
          say "path check of ${dir##*/}: the rig failed the run, so it has no path to check"
          continue
        fi
        say "path check of ${dir##*/}:"
        bun "$here/score.ts" path-check "$dir" || say "the path check of ${dir##*/} could not read its files"
      done
    done
  fi
  for world in "${worlds[@]}"; do bash "$rig" forward-cell "$scenario" "$label" "$world" "$count"; done
}

case $scenario in
owner-walk)
  [ -z "$one_world$path_check$path_control" ] || refuse "--world, --path-check and --path-control are a forward Job's options; $usage"
  # An owner-walk run writes no standin-script.json, so the scripted model would play nothing.
  [ "${SKILL_SCENARIOS_MODEL:-}" != standin-report ] ||
    refuse "SKILL_SCENARIOS_MODEL=standin-report: standin-report is the path check's model, and owner-walk has none; use standin"
  ;;
forward-only | forward-owner)
  [ -z "$one_case" ] || refuse "--case is owner-walk's option; a $scenario Job takes --world"
  forward_job
  exit 0
  ;;
*) refuse "$scenario is no scenario a Job runs: owner-walk, forward-only and forward-owner are" ;;
esac

# ---- owner-walk ----

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

pod_services
model_route "$family"

checkout=$(cd "${SKILL_SCENARIOS_LABEL_CHECKOUT:-$root}" && pwd -P)
commit=$(git -C "$checkout" rev-parse HEAD)
say "$scenario, label group $group, cases ${selected[*]}, label checkout $checkout at $commit, work $SKILL_SCENARIOS_WORK"
bun_install

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
rig_error_tails "$scenario"
[ -n "$one_case" ] || bun "$here/score.ts" cells "$SKILL_SCENARIOS_WORK/runs" "$scenario"
for name in "${selected[@]}"; do
  IFS=- read -r _ form _ <<<"$name"
  cell=$(bash "$rig" cell "$scenario" "$(label_of "$form")" "$name" "$(case_count "$name")")
  printf 'scored-job %s %s %s, label %s: %s\n' "$scenario" "$group" "$name" "$(label_of "$form")" "${cell#*: }"
done
