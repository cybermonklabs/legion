#!/usr/bin/env bash
# label-checkouts.sh: a fresh set of the ablate labels' checkouts, each a clone of its own at one
# commit with its label's cuts applied, and the proof that no text its role reads still states the
# label's rule.
#
#   label-checkouts.sh <repository> <commit> [--hidden <label>]... <patch>...
#
# <repository> is a clone or a worktree of cybermonklabs/legion that holds <commit>. Each <patch> is
# a file named <label>.patch, one for each label that label-rules.ts names (its RULES) and no other,
# and each a diff that `git apply` applies to <commit>. Each `--hidden <label>` names `head` or one
# of those labels: the label `<label>-hidden` gets a checkout of its own with the same content as
# <label>'s. A run of a label whose name ends in `-hidden` hides Dispatch's computed state from its
# agent (rig.sh, hide_state_on), and score.ts leaves unscored a run that reads inside another
# label's checkout, so a hidden label and its source cannot share one. In order:
#   1. it clones <commit> once, with no patch, into a new directory (mktemp -d): the head that the
#      labels are cut from. That directory also gets labels.tsv, a line `<label>\t<checkout>` for
#      each label and each hidden label, so that no file under the labels' own directory names a
#      label;
#   2. it clones <commit> once for each label into a second new directory, under a name of 16
#      random hexadecimal digits that names no label, and applies the label's patch there; and once
#      for each hidden label, with its source's patch (none for head);
#   3. it runs `bun install --force` in the head's checkout and in each of those checkouts, since
#      the rig's profile build of each, the label `head` included, needs it (a frozen install of a
#      fresh clone left the workspace packages' links unresolved), and prints its output only when
#      it fails;
#   4. it runs `rule-search.ts <checkout> cut <label> <head>` over each label's checkout and over
#      the checkout of the label's hidden copy, which fails where a text that the label's role reads
#      still states its rule, or where a sentence that the head's search cuts is still there, and
#      `rule-search.ts <checkout> head <label>...` over head-hidden's, which must be as clean as the
#      head's own search;
#   5. it shows that each hidden checkout holds what its source's holds: the same commit, the same
#      `git diff --binary` and the same `git status --porcelain`.
# It prints the two directories and labels.tsv, and exits 0 only when every step held for every
# label. It removes nothing: the caller deletes the two directories when the runs are done.
set -euo pipefail

me="label-checkouts"
refuse() {
  echo "$me: $*" >&2
  exit 2
}

usage="usage: $me <repository> <commit> [--hidden <label>]... <label>.patch..."
[ $# -ge 3 ] || refuse "$usage"
repository=$1 commit=$2
shift 2
hidden=()
while [ "${1:-}" = --hidden ]; do
  [ $# -ge 2 ] || refuse "$usage"
  hidden+=("$2")
  shift 2
done
[ $# -ge 1 ] || refuse "$usage"
git -C "$repository" rev-parse --verify --quiet "$commit^{commit}" >/dev/null ||
  refuse "$repository holds no commit $commit"

head=$(mktemp -d)
labels=$(mktemp -d)
git clone --quiet --no-checkout "$repository" "$head/head"
git -C "$head/head" checkout --quiet --detach "$commit"
head_install=$(cd "$head/head" && bun install --force 2>&1) ||
  refuse "bun install --force failed in the head's checkout: $head_install"
scenarios=$head/head/packages/pi-legion/scripts/skill-scenarios
search=$scenarios/rule-search.ts
[ -f "$search" ] && [ -f "$scenarios/label-rules.ts" ] ||
  refuse "$commit holds no rule-search.ts and label-rules.ts in packages/pi-legion/scripts/skill-scenarios"

# The labels label-rules.ts names at <commit>, each of which needs exactly one patch.
mapfile -t known < <(bun -e "import { RULES } from '$scenarios/label-rules.ts'; for (const label of Object.keys(RULES)) console.log(label);")
[ ${#known[@]} -gt 0 ] || refuse "label-rules.ts at $commit names no label"
declare -A patch_of=()
for patch in "$@"; do
  label=$(basename -- "$patch" .patch)
  [ -f "$patch" ] || refuse "no file $patch"
  [ -z "${patch_of[$label]:-}" ] || refuse "two patches for $label"
  patch_of[$label]=$(cd "$(dirname -- "$patch")" && pwd -P)/$(basename -- "$patch")
done
for label in "${known[@]}"; do
  [ -n "${patch_of[$label]:-}" ] || refuse "no patch for $label (want ${known[*]})"
done
[ ${#patch_of[@]} -eq ${#known[@]} ] || refuse "a patch names no label of label-rules.ts (its labels: ${known[*]})"
declare -A hidden_of=()
for source in "${hidden[@]}"; do
  [ "$source" = head ] || [ -n "${patch_of[$source]:-}" ] ||
    refuse "--hidden $source names neither head nor a label of label-rules.ts (its labels: ${known[*]})"
  [ -z "${hidden_of[$source]:-}" ] || refuse "--hidden $source is named twice"
  hidden_of[$source]=1
done

# new_checkout LABEL SOURCE: a clone of <commit> under a random name, with SOURCE's patch applied
# (none for head), installed, its line in labels.tsv, and its place in checkout_of and made.
tsv=$head/labels.tsv
: >"$tsv"
declare -A checkout_of=([head]=$head/head)
made=()
new_checkout() {
  local label=$1 source=$2 name checkout log
  name=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  checkout=$labels/$name
  git clone --quiet --no-checkout "$repository" "$checkout"
  git -C "$checkout" checkout --quiet --detach "$commit"
  if [ "$source" != head ]; then
    git -C "$checkout" apply "${patch_of[$source]}"
    [ -n "$(git -C "$checkout" status --porcelain)" ] || refuse "the patch of $source changes nothing at $commit"
  fi
  log=$(cd "$checkout" && bun install --force 2>&1) || refuse "bun install --force failed in $label's checkout: $log"
  printf '%s\t%s\n' "$label" "$checkout" >>"$tsv"
  checkout_of[$label]=$checkout
  made+=("$label")
}
for label in "${known[@]}"; do new_checkout "$label" "$label"; done
for source in "${hidden[@]}"; do new_checkout "$source-hidden" "$source"; done

# content CHECKOUT: what step 5 compares, the commit, the diff and the status of CHECKOUT.
content() {
  git -C "$1" rev-parse HEAD
  git -C "$1" diff --binary
  git -C "$1" status --porcelain
}

failed=0
for label in "${made[@]}"; do
  checkout=${checkout_of[$label]}
  source=${label%-hidden}
  if [ "$source" != "$label" ]; then
    source_checkout=${checkout_of[$source]}
    if [ "$(content "$checkout")" = "$(content "$source_checkout")" ]; then
      echo "$me: $label: the same commit, diff and status as $source's checkout"
    else
      failed=1
      echo "$me: $label: FAULT, its checkout $checkout holds other content than $source's ($source_checkout)" >&2
    fi
  fi
  if [ "$source" = head ]; then
    if bun "$search" "$checkout" head "${known[@]}" >"$head/search-$label.txt" 2>&1; then
      echo "$me: $label: rule-search.ts head ${known[*]}: 0 faults"
    else
      failed=1
      echo "$me: $label: FAULT, rule-search.ts head exited non-zero ($head/search-$label.txt):" >&2
      grep -E '^FAULT' "$head/search-$label.txt" >&2 || true
    fi
  elif bun "$search" "$checkout" cut "$source" "$head/head" >"$head/cut-$label.txt" 2>&1; then
    echo "$me: $label: $(grep -E "^$source at " "$head/cut-$label.txt")"
  else
    failed=1
    echo "$me: $label: FAULT, rule-search.ts cut exited non-zero ($head/cut-$label.txt):" >&2
    grep -E '^FAULT' "$head/cut-$label.txt" >&2 || true
  fi
done

echo "$me: head $head/head, labels $labels, list $tsv"
exit "$failed"
