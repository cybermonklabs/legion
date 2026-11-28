#!/usr/bin/env bash
# label-checkouts.sh: a fresh set of the ablate labels' checkouts, each a clone of its own at one
# commit with its label's patch applied.
#
#   label-checkouts.sh <repository> <commit> [--hidden <label>]... <patch>...
#
# <repository> is a clone or a worktree of cybermonklabs/legion that holds <commit>. Each <patch> is
# a file named <label>.patch, one for each label, and each a diff that `git apply` applies to
# <commit>: the labels are the patches' names. Each `--hidden <label>` names `head` or one of those
# labels: the label `<label>-hidden` gets a checkout of its own with the same content as
# <label>'s. A run of a label whose name ends in `-hidden` hides Dispatch's computed state from its
# agent (rig.sh, hide_state_on), and score.ts leaves unscored a run that reads inside another
# label's checkout, so a hidden label and its source cannot share one. That a label's patch cuts
# the rule it names is shown by its runs (the ablation scores below the head's), not by a search of
# the patched texts here. In order:
#   1. it clones <commit> once, with no patch, into a new directory (mktemp -d): the head that the
#      labels are cut from. That directory also gets labels.tsv, a line `<label>\t<checkout>` for
#      each label and each hidden label, so that no file under the labels' own directory names a
#      label;
#   2. it clones <commit> once for each label into a second new directory, under a name of 16
#      random hexadecimal digits that names no label, checks that the label's patch applies there
#      (`git apply --check`) and changes something, and applies it; and once for each hidden
#      label, with its source's patch (none for head);
#   3. it runs `bun install --force` in the head's checkout and in each of those checkouts, since
#      the rig's profile build of each, the label `head` included, needs it (a frozen install of a
#      fresh clone left the workspace packages' links unresolved), and prints its output only when
#      it fails;
#   4. it shows that each hidden checkout holds what its source's holds: the same commit, the same
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

# The labels are the patches' names, each with exactly one patch.
declare -A patch_of=()
known=()
for patch in "$@"; do
  label=$(basename -- "$patch" .patch)
  [ -f "$patch" ] || refuse "no file $patch"
  [ "$label" != head ] || refuse "a patch may not be named head.patch"
  [ -z "${patch_of[$label]:-}" ] || refuse "two patches for $label"
  patch_of[$label]=$(cd "$(dirname -- "$patch")" && pwd -P)/$(basename -- "$patch")
  known+=("$label")
done
declare -A hidden_of=()
for source in "${hidden[@]}"; do
  [ "$source" = head ] || [ -n "${patch_of[$source]:-}" ] ||
    refuse "--hidden $source names neither head nor a label of the patches (their labels: ${known[*]})"
  [ -z "${hidden_of[$source]:-}" ] || refuse "--hidden $source is named twice"
  hidden_of[$source]=1
done

head=$(mktemp -d)
labels=$(mktemp -d)
git clone --quiet --no-checkout "$repository" "$head/head"
git -C "$head/head" checkout --quiet --detach "$commit"
head_install=$(cd "$head/head" && bun install --force 2>&1) ||
  refuse "bun install --force failed in the head's checkout: $head_install"

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
    log=$(git -C "$checkout" apply --check "${patch_of[$source]}" 2>&1) ||
      refuse "the patch of $source does not apply at $commit: $log"
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

# content CHECKOUT: what step 4 compares, the commit, the diff and the status of CHECKOUT.
content() {
  git -C "$1" rev-parse HEAD
  git -C "$1" diff --binary
  git -C "$1" status --porcelain
}

failed=0
for label in "${made[@]}"; do
  source=${label%-hidden}
  [ "$source" != "$label" ] || continue
  checkout=${checkout_of[$label]}
  source_checkout=${checkout_of[$source]}
  if [ "$(content "$checkout")" = "$(content "$source_checkout")" ]; then
    echo "$me: $label: the same commit, diff and status as $source's checkout"
  else
    failed=1
    echo "$me: $label: FAULT, its checkout $checkout holds other content than $source's ($source_checkout)" >&2
  fi
done

echo "$me: head $head/head, labels $labels, list $tsv"
exit "$failed"
