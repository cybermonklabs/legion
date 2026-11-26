#!/usr/bin/env bash
# The check c-spelling-gone: the decisions document and the `decisions` design step
# are gone from every file that writes or asks for them, read outside the server's own tests. It
# needs no server and no database: it reads this checkout and runs this branch's own `legion`.
#
# The first sweep is literal and covers production code alone. It reads the checkout
# (`git grep`, tracked and untracked files alike, ignored ones left out) for `decisions.md`, the
# `decisions-md` slug, ArtifactRoleDecisions, GatedStepDecisions and DesignStepDecisions, a
# `decisions.md` preceded by `parked-` or `past-` being no hit, and prints each hit as one of:
#     kept <class> <path>              a place listed as kept rather than swept: the catalog-only
#                                      migration that creates decisions_documents(), a changelog,
#                                      test material that must still exercise the old document, or
#                                      the header comment of the applied migration
#                                      20261101162254_decision_records.up.sql
#     admitted <path>:<n> serves <item>  a line that serves a thing this child keeps for one
#                                      release: the line or the one above it carries
#                                      `kept for one release: <id>`, an id root-deletes.txt lists
#                                      (the plan's "For the root to delete"), printed with its
#                                      thing; or the reading of a retired document's slug
#                                      (refs.RetiredDecisionsSlug), or a citation
#                                      dispatch://KEY/artifact/decisions-md in
#                                      packages/envoy/cmd/listener/api.go or
#                                      packages/daemon/internal/pause/pause.go
#     hit <path>:<n>: <line>           a production line that still spells one, which fails
# The step's Go code calls decisions_documents() and spells no slug, so no line under the id
# `decisions-step` is admitted. Before it reads the checkout, the sweep reads a planted instance
# through the same search and the same classification: a scratch git repository under the run's
# own directory holding a copy of the first inventoried file the sweep counts as production, with
# one line spelling decisions.md added. It prints each hit there as `planted hit <path>:<n>: <line>`
# and fails unless the added line is one of them, so a zero over the checkout is never the reading
# of a search that cannot find a production line.
#
# The third sweep is R11's: `git grep -n` for the word decisions in double quotes over
# packages/contracts/src, packages/dispatch/web/src and packages/dispatch/e2e. It admits a line by
# its text, never a whole file, and only a line that serves what R11 keeps: the Decisions view, its
# tab, its dispatch://KEY/decisions route and its query key; GATED_STEPS and DocumentRoleSchema,
# which keep the value for one release as a value they read; and the generated step types, since
# the gated-steps write still admits it. A line that is the word alone, in a list or a union, is
# admitted by the declaration it stands in as well (PRESET_ORDER, PLAIN_ISSUE_TABS and IssueTab,
# and a test's TreeRead and TREE_READS). A test's line is admitted only where it exercises one of
# those, or feeds the client a stored step or a document role GATED_STEPS or DocumentRoleSchema
# still reads, and so is the base release's gated steps seed-decisions.sh publishes, which must
# still hold the step. It prints each admitted line with what it serves, and fails on any other:
# gated-steps.ts, or a step list of gated-step-approvals.e2e.ts, among them. Before it reads the
# checkout it reads a planted instance through the same search and the same admission, as the first
# sweep does: a scratch git repository under the run's own directory holding a copy of
# gated-steps.ts, of each file it admits lines of the view, its tab or its route from
# (DecisionsPanel.tsx, issue-tab-list.ts, IssuePage.tsx, page-words.ts and routes.ts) and of a web
# test (gated-steps.test.ts), each with one line sending the step added, printed as
# `planted hit …`, each of which must be reported as a hit. It then reads the daemon's
# contract, which narrows now: legionDesignStep in packages/contracts/src/legion-api.ts (through
# the two schemas that export it, LegionGateRegisterRequest's step and
# LegionControllerSecretResponse's designGateSteps, which bun parses) and the `register_gate` step
# in packages/pi-legion/src/tools.ts must refuse `decisions`; a planted `spec` is taken by both
# schemas, so a refusal of everything fails too.
#
# The second sweep is about meaning. decisions-inventory.txt holds one line for each skill and
# prompt file: its path, a tab, and whether it tells an agent to move a person's decision out of a
# spec (`yes` or `no`). A path that names an issue key of the private tracker stands there as
# `sha256:` and the SHA-256 of the path, so the run looks a file up by its path and then by that
# hash. The run fails when one file of skills/, docs/ or
# packages/daemon/internal/prompts/, the root AGENTS.md, an AGENTS.md or README.md under packages/,
# packages/pi-envoy/test/decisions-materiality.eval.md,
# packages/pi-legion/scripts/skill-scenarios/judge-prompts.ts or
# deploy/kubernetes/daemon/legion.yaml.example is missing from it under either, and when a line
# says `yes`.
#
# It then reads the route descriptions of the Decisions read and its Keep in
# packages/envoy/internal/dispatch/api/routes_table.go and the published page
# docs/site/src/content/docs/dispatch/issues-and-projects.md, sentence by sentence, and fails on a
# sentence that still calls the view a reading of the decisions document. The same matcher is run
# first over the three sentences the release before wrote there, and must flag each, so a matcher
# that flags nothing fails rather than passes.
#
# Last it builds `legion` from packages/daemon/cmd/legion into a directory of its own under
# TMPDIR, linked with the commit this checkout stands on (`-X main.revision`, which `legion
# version` prints, as the worker image and the release link theirs); LEGION_BINARY names a build to
# run in its place, which a negative control's scratch copy makes and links the same way. The
# legion it runs must report, in `legion version`, the commit `git rev-parse HEAD` reads in this
# checkout, and must not be /opt/legion/bin/legion or the legion PATH finds, or the run fails. It
# runs `legion config check` three times: over a legion.yaml it accepts as written
# (`gates.design: [spec]`), printing that acceptance; over a copy whose gates.design holds
# decisions, criteria and spec, which must be refused, the refusal naming gates.design and the
# steps that remain (criteria and spec), since readDesignSteps in
# packages/daemon/internal/config/stage3.go holds that list to record.DesignSteps; and over one
# holding the remaining steps, which must be accepted. A copy holding decisions that is accepted
# fails printing the accepted line.
#
# Each passing check prints `PASS: <section> <what>`. A sweep prints every line it found, and then
# the first failing check prints `FAIL: <section> <what>: <observed>` on stderr and the run exits
# 1; a missing input exits 2. It prints no SKIP line. Every check is `cmd || fail …`, never part
# of an `&&` list: under `set -e` a failing command anywhere but last in such a list does not stop
# the script.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: [LEGION_BINARY=<a legion build>] bash e2e/acceptance/decisions-gone.sh
       (run from packages/dispatch; it reads the checkout this file is part of and builds that
       checkout's packages/daemon/cmd/legion under TMPDIR unless LEGION_BINARY names a build,
       which must report the checkout's HEAD in `legion version`: build it with
       go build -ldflags "-X main.revision=$(git rev-parse HEAD)" ./cmd/legion)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_commands bun git go grep sed sha256sum
require_env TMPDIR
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
root="$(git -C "$script_dir" rev-parse --show-toplevel)" ||
  fail setup "find the checkout" "git rev-parse --show-toplevel failed in $script_dir"
readonly root
readonly inventory="$script_dir/decisions-inventory.txt"
readonly root_deletes="$script_dir/root-deletes.txt"
[[ -r "$inventory" && -r "$root_deletes" ]] ||
  fail setup "read the inventory and the root's list" "$inventory or $root_deletes cannot be read"

# shellcheck disable=SC2031 # this shell's own TMPDIR
work="$(mktemp -d -p "$TMPDIR")"
readonly work
cleanup() {
  local code=$?
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The quoted word, built so that this file does not spell it itself.
readonly dq='"'
readonly quoted_word="${dq}decisions${dq}"

# The tree the sweeps read: the checkout, but while the first or the third sweep reads its planted
# instance, the scratch tree that holds it.
sweep_root="$root"

# sweep_grep PATTERN PATHSPEC…: `git grep -n -I -E` over sweep_root's tracked and untracked files,
# ignored ones left out, each hit `path:line:text` in $work/hits. No hit is no error.
sweep_grep() {
  local pattern="$1"
  shift
  local code=0
  git -C "$sweep_root" grep --untracked -n -I -E -e "$pattern" -- "$@" >"$work/hits" 2>"$work/grep" || code=$?
  [[ "$code" -le 1 ]] || fail sweep "git grep $pattern" "exited $code: $(<"$work/grep")"
}

# ---------------------------------------------------------------------------------------------
# The first sweep: the five strings, production code alone.
# ---------------------------------------------------------------------------------------------
readonly literal='decisions\.md|decisions-md|ArtifactRoleDecisions|GatedStepDecisions|DesignStepDecisions'
readonly catalog_migration=packages/envoy/internal/dispatch/store/migrations/20261117093337_decisions_documents.up.sql
readonly applied_migration=packages/envoy/internal/dispatch/store/migrations/20261101162254_decision_records.up.sql
# The citation packages/envoy/cmd/listener/api.go and packages/daemon/internal/pause/pause.go
# carry: a retired decisions document's address under any issue key.
readonly retired_citation='dispatch://[A-Z][A-Z0-9]*-[0-9]+/artifact/decisions-md'

# The ids the root's list holds, with their things.
declare -A kept_items=()
while IFS=$'\t' read -r id thing _; do
  [[ -n "$id" && "$id" != \#* ]] || continue
  kept_items["$id"]="$thing"
done <"$root_deletes"
[[ "${#kept_items[@]}" -gt 0 ]] || fail literal "read the root's list" "$root_deletes lists no id"

# The kept migration is the one that creates decisions_documents(), and the applied one is the one
# that created decision_keeps: a kept path that stopped being what it is kept for would hide a hit.
grep -Eqi 'create (or replace )?function decisions_documents' "$root/$catalog_migration" ||
  fail literal "the kept catalog migration creates decisions_documents()" "$catalog_migration does not"
grep -Eqi 'create table decision_keeps' "$root/$applied_migration" ||
  fail literal "the kept applied migration created decision_keeps" "$applied_migration does not"

# kept_class PATH: the class of a kept path, or nothing.
kept_class() {
  case "$1" in
    "$catalog_migration") printf 'catalog-only-migration' ;;
    */CHANGELOG.md | CHANGELOG.md | */.changes/*) printf 'changelog' ;;
    *_test.go | *.test.ts | *.test.tsx | */testdata/* | packages/dispatch/e2e/acceptance/*) printf 'test-material' ;;
  esac
}

# header_comment PATH LINE: LINE of PATH is in the run of `--` lines the file opens with.
header_comment() {
  local number=0 text
  while IFS= read -r text; do
    number=$((number + 1))
    [[ "$text" == --* ]] || return 1
    [[ "$number" -lt "$2" ]] || return 0
  done <"$sweep_root/$1"
  return 1
}

# kept_comment_id PATH LINE: the id of the `kept for one release:` comment on LINE of PATH or the
# line above it, or nothing.
kept_comment_id() {
  local from=$(($2 - 1))
  [[ "$from" -ge 1 ]] || from=1
  sed -n "${from},${2}p" "$sweep_root/$1" |
    sed -nE 's/.*kept for one release: ([a-z0-9-]+).*/\1/p' | tail -n 1
}

# literal_sweep: the first sweep over sweep_root, its kept paths in kept_paths (path to class), the
# production lines that spell one of the strings in hits, and how many lines it found and admitted
# in literal_seen and admitted. It prints each admitted line.
literal_sweep() {
  local hit path rest number text stripped class id
  declare -gA kept_paths=()
  hits=()
  admitted=0
  literal_seen=0
  sweep_grep "$literal" .
  while IFS= read -r hit; do
    path="${hit%%:*}"
    rest="${hit#*:}"
    number="${rest%%:*}"
    text="${rest#*:}"
    stripped="${text//parked-decisions.md/}"
    stripped="${stripped//past-decisions.md/}"
    [[ "$stripped" =~ $literal ]] || continue
    literal_seen=$((literal_seen + 1))
    class="$(kept_class "$path")"
    if [[ -z "$class" && "$path" == "$applied_migration" ]] && header_comment "$path" "$number"; then
      class=applied-migration-header
    fi
    if [[ -n "$class" ]]; then
      kept_paths["$path"]="$class"
      continue
    fi
    if [[ "$path" == packages/envoy/internal/dispatch/refs/graph.go &&
      "$text" =~ RetiredDecisionsSlug\ =\ \"decisions-md\" ]]; then
      printf 'admitted %s:%s serves the reading of a retired document'"'"'s slug (refs.RetiredDecisionsSlug)\n' "$path" "$number"
      admitted=$((admitted + 1))
      continue
    fi
    if [[ ("$path" == packages/envoy/cmd/listener/api.go || "$path" == packages/daemon/internal/pause/pause.go) &&
      "$text" =~ $retired_citation ]]; then
      printf 'admitted %s:%s serves a citation dispatch://KEY/artifact/decisions-md, a valid citation of a retired document\n' "$path" "$number"
      admitted=$((admitted + 1))
      continue
    fi
    id="$(kept_comment_id "$path" "$number")"
    if [[ -n "$id" && "$id" != decisions-step && -n "${kept_items[$id]:-}" ]]; then
      printf 'admitted %s:%s serves %s: %s\n' "$path" "$number" "$id" "${kept_items[$id]}"
      admitted=$((admitted + 1))
      continue
    fi
    hits+=("$path:$number: $text")
  done <"$work/hits"
}

# The planted instance, read first and by the same sweep: a scratch tree of its own (a git
# repository the run makes under $work) holding a copy of the first file of the inventory that
# the sweep counts as production, with one line added that spells the decisions document. The
# sweep must report that line as a hit before its zero over the checkout is read; a sweep that
# misses it, by a wrong pattern, a wrong tree or a classification that keeps or admits a
# production line, would read zero over any checkout.
planted_path=""
while IFS=$'\t' read -r path _; do
  [[ -n "$path" && "$path" != \#* && -f "$root/$path" && -z "$(kept_class "$path")" ]] || continue
  planted_path="$path"
  break
done <"$inventory"
[[ -n "$planted_path" ]] || fail literal "find an inventoried production file to plant the spelling in" "$inventory names none that the checkout holds"
mkdir -p "$work/planted/$(dirname -- "$planted_path")"
cp -- "$root/$planted_path" "$work/planted/$planted_path"
printf '\nThe planted line moves the answered asks into %s.\n' "decisions.md" >>"$work/planted/$planted_path"
planted_line="$(wc -l <"$work/planted/$planted_path")"
git -C "$work/planted" init -q ||
  fail literal "make the planted instance's scratch repository" "git init failed in $work/planted"
sweep_root="$work/planted"
literal_sweep
sweep_root="$root"
planted_found=0
for hit in "${hits[@]}"; do
  printf 'planted hit %s\n' "$hit"
  [[ "$hit" != "$planted_path:$planted_line: "* ]] || planted_found=1
done
[[ "$planted_found" == 1 ]] ||
  fail literal "the sweep reports the planted line $planted_path:$planted_line as a hit" \
    "it reported ${#hits[@]} hits there, ${#kept_paths[@]} kept paths and $admitted admitted lines: the search is broken"
pass literal "the sweep reports the planted line $planted_path:$planted_line, a copy of an inventoried file under $work/planted, as a hit"

# The real search, over the checkout.
literal_sweep
for path in "${!kept_paths[@]}"; do
  printf 'kept %s %s\n' "${kept_paths[$path]}" "$path"
done | sort
for hit in "${hits[@]}"; do
  printf 'hit %s\n' "$hit"
done
# The search's own control over the checkout: the kept test material spells the strings, so a sweep
# that found nothing searched nothing.
[[ "${#kept_paths[@]}" -gt 0 ]] ||
  fail literal "the sweep reads the checkout" "it found no hit at all, not even in the kept test material: the search is broken"
pass literal "the sweep found $literal_seen lines in the checkout, ${#kept_paths[@]} kept paths and $admitted admitted lines among them"
[[ "${#hits[@]}" -eq 0 ]] ||
  fail literal "no production file spells the decisions document or step" "${#hits[@]} lines still do, each printed above as hit"
pass literal "no production file spells decisions.md, decisions-md, ArtifactRoleDecisions, GatedStepDecisions or DesignStepDecisions"

# ---------------------------------------------------------------------------------------------
# The third sweep: R11's, over the contracts, the web app and the e2e tree.
# ---------------------------------------------------------------------------------------------
# enclosing_declaration PATH NUMBER: the name of the declaration that line NUMBER of PATH, under
# sweep_root, stands in: the nearest line at or above it that opens one at the left margin (`const`,
# `let`, `type`, `interface`, `function` or `enum`, exported or not), or nothing. A line that is the
# word alone, in a list or a union, says nothing of what it serves; the declaration it is part of
# does.
enclosing_declaration() {
  sed -n "1,${2}p" "$sweep_root/$1" |
    sed -nE 's/^(export )?(declare )?(const|let|type|interface|function|enum) ([A-Za-z_$][A-Za-z0-9_$]*).*/\4/p' |
    tail -n 1
}

# step_admission PATH NUMBER TEXT: what R11 keeps line NUMBER of PATH, whose text is TEXT, for, or
# nothing. Each line is admitted by its text, and a line that is the word alone by the declaration
# it stands in too, never by its file: any other line of the same file that spells the word is a
# hit. A test's line is admitted only where it exercises a thing R11 keeps, or feeds the client a
# value GATED_STEPS or DocumentRoleSchema still reads.
step_admission() {
  local path="$1" number="$2" text="$3" bare declaration=""
  case "$path" in
    packages/dispatch/web/src/features/issue/gated-steps.ts | */gated-step-approvals.e2e.ts) return 0 ;;
  esac
  if [[ "$text" == *"[${quoted_word}, "* && "$text" == *[Qq]uery* ]]; then
    printf 'the query key of the Decisions view'
    return 0
  fi
  # The line without its indentation and its trailing spaces, for a line that is the word alone.
  bare="${text#"${text%%[![:space:]]*}"}"
  bare="${bare%"${bare##*[![:space:]]}"}"
  if [[ "$bare" == "${quoted_word}," || "$bare" == "| ${quoted_word}" ]]; then
    declaration="$(enclosing_declaration "$path" "$number")"
  fi
  case "$path" in
    packages/dispatch/web/src/features/issue/issue-tab-list.ts)
      [[ "$text" != *"{ id: ${quoted_word}, label: ${dq}Decisions${dq} }"* ]] || printf 'the Decisions tab' ;;
    packages/dispatch/web/src/components/Tabs.test.tsx)
      [[ "$text" != *"{ id: ${quoted_word}, label: ${dq}Decisions${dq} }"* ]] || printf 'test material: the Decisions tab' ;;
    packages/dispatch/web/src/features/issue/IssuePage.tsx)
      [[ "$text" != *"activeTab === ${quoted_word}"* && "$text" != *"activeTab !== ${quoted_word}"* &&
        "$text" != *"label: ${dq}Decisions tab${dq}, tab: ${quoted_word} }"* ]] || printf 'the Decisions tab' ;;
    packages/dispatch/web/src/features/issue-page/page-words.ts)
      if [[ "$bare" == "${quoted_word}," && "$declaration" == PRESET_ORDER ]]; then
        printf 'the Decisions tab of the filter row (PRESET_ORDER)'
      fi
      ;;
    # Main's new issue page model (#4196): its Decisions filter and the decisions read's query
    # key. Each is admitted by its text, so any other line of these files that spells the word is
    # still a hit.
    packages/dispatch/web/src/features/issue-page/issue-address.ts)
      [[ "$text" != *"decisions: ${quoted_word},"* ]] || printf 'the Decisions filter of the issue page'"'"'s address' ;;
    packages/dispatch/web/src/features/issue-page/presets.ts)
      [[ "$text" != *"view(${quoted_word})"* ]] || printf 'the Decisions filter of the issue page' ;;
    packages/dispatch/web/src/features/issue-page/useIssueTree.ts)
      [[ "$text" != *"[${quoted_word}, key]"* ]] || printf 'the query key of the Decisions read' ;;
    # Main's conversation rows (#4192): the decision node's actions name the Decisions read's query
    # key they read from, admitted by its text as #4196's are.
    packages/dispatch/web/src/features/issue-page/DecisionActions.tsx)
      [[ "$text" != *"[${quoted_word}, <key>]"* ]] || printf 'the query key of the Decisions read' ;;
    # Main's new issue page header (#4226): its count's press opens the Decisions filter's address.
    packages/dispatch/web/src/features/issue-page/IssuePropertiesRow.tsx)
      [[ "$text" != *"presetPath(issue.key, ${quoted_word})"* ]] ||
        printf 'the Decisions filter of the issue page'"'"'s address' ;;
    # Main's new issue page frame (#4189): its keymap's Decisions tab, and the e2e rows that press
    # the Decisions preset, read its address and list the views and shortcuts that name it. Each is
    # admitted by its text, as #4196's are, so any other line of these files that spells the word
    # is still a hit.
    packages/dispatch/web/src/features/issue-page/useIssueKeymap.ts)
      [[ "$text" != *"toggled(key, ${quoted_word}"* && "$text" != *"tab: ${quoted_word},"* ]] ||
        printf 'the Decisions tab of the issue page'"'"'s keymap' ;;
    packages/dispatch/e2e/issue-page-d.e2e.ts)
      [[ "$text" != *"presetToggle(page, ${quoted_word})"* &&
        "$text" != *"addressOf(key, ${quoted_word})"* &&
        "$text" != *"[${dq}d${dq}, ${quoted_word}]"* &&
        "$text" != *"[${dq}spec${dq}, ${dq}verification${dq}, ${dq}plan${dq}, ${quoted_word}]"* ]] ||
        printf 'test material: the Decisions tab of the issue page' ;;
    # Main's Decisions view (#4225): its slot draws the Decisions filter, its stage fixture and
    # its tests draw that view at its address, its e2e rows open it, and the body's drawn test puts
    # the Decisions read in the cache under its query key on a line of its own. Each is admitted by
    # its text, as #4196's are, so any other line of these files that spells the word is still a
    # hit.
    packages/dispatch/web/src/features/issue-page/slots/view-decisions.tsx)
      [[ "$text" != *"presetView(root, ${quoted_word}, "* ]] || printf 'the Decisions filter of the issue page' ;;
    packages/dispatch/web/src/features/issue-page/slots/stage-fixtures.tsx)
      [[ "$bare" != "preset = ${quoted_word}," ]] || printf 'test material: the Decisions filter of the issue page' ;;
    packages/dispatch/web/src/features/issue-page/slots/stage.test.tsx | packages/dispatch/web/src/features/issue-page/decisions-filter/view-decisions-slot.test.tsx)
      [[ "$text" != *"presetPath(KEY, ${quoted_word})"* ]] ||
        printf 'test material: the Decisions filter of the issue page'"'"'s address' ;;
    packages/dispatch/web/src/features/issue-page/decisions-filter/DecisionsFilterBody.drawn.test.tsx)
      if [[ "$text" =~ presetView\(.*,\ ${dq}decisions${dq}, ]]; then
        printf 'test material: the Decisions filter of the issue page'
      elif [[ "$bare" == "[${quoted_word}, KEY]," ]]; then
        printf 'test material: the query key of the Decisions view'
      fi
      ;;
    packages/dispatch/e2e/issue-page-q.e2e.ts)
      [[ "$text" != *"openPreset(page, seeded.key, ${quoted_word})"* &&
        "$text" != *"presetPath(seeded.key, ${quoted_word})"* &&
        "$text" != *"presetPath(key, ${quoted_word})"* ]] ||
        printf 'test material: the Decisions filter of the issue page' ;;
    packages/dispatch/web/src/features/refs/routes.ts)
      if [[ "$text" == *"| { key: string; kind: ${quoted_word} }"* ]]; then
        printf 'the dispatch://KEY/decisions route (IssueRoute)'
      elif [[ "$bare" == "| ${quoted_word}" && "$declaration" == IssueTab ]]; then
        printf 'the Decisions tab the dispatch://KEY/decisions route opens (IssueTab)'
      elif [[ "$bare" == "${quoted_word}," && "$declaration" == PLAIN_ISSUE_TABS ]]; then
        printf 'the dispatch://KEY/decisions route (PLAIN_ISSUE_TABS)'
      fi
      ;;
    packages/dispatch/web/src/features/refs/routes.test.ts)
      [[ "$text" != *"kind: ${quoted_word} as const"* && "$text" != *"issueTabForRoute(route, undefined)).toBe(${quoted_word})"* ]] ||
        printf 'test material: the dispatch://KEY/decisions route' ;;
    packages/dispatch/web/src/features/issue/DecisionsPanel.test.tsx)
      [[ "$text" != *"DecisionsRead[${quoted_word}]"* ]] ||
        printf 'test material: the old decisions member of the Decisions view'"'"'s read, kept for one release (compat-objects)' ;;
    packages/dispatch/web/src/__tests__/event-feed.test.ts)
      [[ "$text" != *"decisions: { key: [${quoted_word}, "* && "$text" != *"spares: [${quoted_word}],"* ]] ||
        printf 'test material: the query key of the Decisions view' ;;
    packages/dispatch/web/src/features/issue-page/IssuePresets.test.tsx)
      [[ ! "$bare" =~ ^\[${dq}\{[A-Za-z]+\}${dq},\ ${dq}decisions${dq}\],$ ]] ||
        printf 'test material: the Decisions tab of the filter row' ;;
    packages/dispatch/web/src/features/issue-page/presets.test.ts | packages/dispatch/web/src/features/issue-page/decisions-filter/decisions-parts.test.ts | packages/dispatch/web/src/features/issue-page/decisions-filter/decisions-entries.test.tsx)
      [[ ! "$text" =~ presetView\(.*,\ ${dq}decisions${dq}, && "$text" != *"drawn(root, ${quoted_word})"* ]] ||
        printf 'test material: the Decisions filter of the issue page' ;;
    packages/dispatch/web/src/features/issue-page/issue-address.test.ts)
      [[ "$text" != *"presetPath(${dq}CORE-1${dq}, ${quoted_word})"* &&
        "$text" != *"[${dq}spec${dq}, ${dq}verification${dq}, ${dq}plan${dq}, ${quoted_word}]"* ]] ||
        printf 'test material: the Decisions filter of the issue page'"'"'s address' ;;
    packages/dispatch/web/src/features/issue-page/useIssueTree.test.tsx)
      if [[ ("$bare" == "| ${quoted_word}" && "$declaration" == TreeRead) ||
        ("$bare" == "${quoted_word}," && "$declaration" == TREE_READS) || "$text" == *"answered(${quoted_word});"* ]]; then
        printf 'test material: the Decisions read the issue tree waits on'
      elif [[ "$text" == *"{ role: ${quoted_word} }"* ]]; then
        printf 'test material: a decisions document an earlier release stored, whose role DocumentRoleSchema still reads (contract-read-side)'
      fi
      ;;
    packages/dispatch/web/src/features/issue-page/issue-tree.test.ts)
      [[ "$bare" != "role: ${quoted_word}," ]] ||
        printf 'test material: a decisions document an earlier release stored, whose role DocumentRoleSchema still reads (contract-read-side)' ;;
    packages/dispatch/web/src/features/issue/gated-steps.test.ts)
      [[ "$text" != *"gatedStepEntries(gating(${quoted_word}, "* ]] ||
        printf 'test material: a stored step list holding the retired step, which GATED_STEPS still reads (contract-read-side)' ;;
    packages/contracts/src/legion-roles.ts)
      [[ "$text" != *"GATED_STEPS ="* ]] || printf 'GATED_STEPS, kept for one release (contract-read-side)' ;;
    packages/contracts/src/dispatch-api-documents.ts)
      [[ "$text" != *"DocumentRoleSchema ="* ]] || printf 'DocumentRoleSchema, kept for one release (contract-read-side)' ;;
    packages/contracts/src/dispatch-api.generated.ts)
      [[ ! "$text" =~ steps?:\ (null\ \|\ \()?${dq}decisions${dq} ]] ||
        printf 'the generated step types, since the gated-steps write still admits the step (steps-write-admits)' ;;
    packages/dispatch/e2e/acceptance/seed-decisions.sh)
      [[ "$text" != *steps* ]] || printf 'test material: the base release'"'"'s gated steps, which must still hold the old step' ;;
  esac
}

# step_sweep: the third sweep over sweep_root, each line R11 keeps printed as admitted and counted
# in step_admitted, every other line that spells the word in step_hits.
step_sweep() {
  local hit path rest number text serves
  step_hits=()
  step_admitted=0
  sweep_grep "$quoted_word" packages/contracts/src packages/dispatch/web/src packages/dispatch/e2e
  while IFS= read -r hit; do
    path="${hit%%:*}"
    rest="${hit#*:}"
    number="${rest%%:*}"
    text="${rest#*:}"
    serves="$(step_admission "$path" "$number" "$text")"
    if [[ -n "$serves" ]]; then
      printf 'admitted %s:%s serves %s\n' "$path" "$number" "$serves"
      step_admitted=$((step_admitted + 1))
      continue
    fi
    step_hits+=("$path:$number: $text")
  done <"$work/hits"
}

# The planted instance, read first and by the same sweep: a scratch git repository of its own under
# $work holding a copy of gated-steps.ts, a caller R11 names, and of each file the sweep admits
# lines of the view, its tab or its route from - and of a web test - each with one line added that
# sends the step. The line is spelled here apart from quoted_word, so a misspelt pattern does not
# plant its own misspelling. The sweep must report each of those lines as a hit before its zero over
# the checkout is read: a sweep that admitted a whole file, or every web test, would admit them.
readonly -a planted_step_paths=(
  packages/dispatch/web/src/features/issue/gated-steps.ts
  packages/dispatch/web/src/features/issue/DecisionsPanel.tsx
  packages/dispatch/web/src/features/issue/issue-tab-list.ts
  packages/dispatch/web/src/features/issue/IssuePage.tsx
  packages/dispatch/web/src/features/issue-page/page-words.ts
  packages/dispatch/web/src/features/refs/routes.ts
  packages/dispatch/web/src/features/issue/gated-steps.test.ts
)
declare -A planted_step_line=()
for planted_step_path in "${planted_step_paths[@]}"; do
  [[ -f "$root/$planted_step_path" ]] ||
    fail step "read $planted_step_path to plant the step in" "the checkout holds no such file"
  mkdir -p "$work/planted-step/$(dirname -- "$planted_step_path")"
  cp -- "$root/$planted_step_path" "$work/planted-step/$planted_step_path"
  printf '\nexport const plantedStep = %sdecisions%s;\n' "$dq" "$dq" >>"$work/planted-step/$planted_step_path"
  planted_step_line["$planted_step_path"]="$(wc -l <"$work/planted-step/$planted_step_path")"
done
git -C "$work/planted-step" init -q ||
  fail step "make the planted instance's scratch repository" "git init failed in $work/planted-step"
sweep_root="$work/planted-step"
step_sweep
sweep_root="$root"
for hit in "${step_hits[@]}"; do
  printf 'planted hit %s\n' "$hit"
done
for planted_step_path in "${planted_step_paths[@]}"; do
  planted_found=0
  for hit in "${step_hits[@]}"; do
    [[ "$hit" != "$planted_step_path:${planted_step_line[$planted_step_path]}: "* ]] || planted_found=1
  done
  [[ "$planted_found" == 1 ]] ||
    fail step "the sweep reports the planted line $planted_step_path:${planted_step_line[$planted_step_path]} as a hit" \
      "it reported ${#step_hits[@]} hits there and $step_admitted admitted lines: a line of that file is admitted whatever it says"
  pass step "the sweep reports the planted line $planted_step_path:${planted_step_line[$planted_step_path]}, a copy under $work/planted-step, as a hit"
done

# The real search, over the checkout.
step_sweep
for hit in "${step_hits[@]}"; do
  printf 'hit %s\n' "$hit"
done
[[ "$step_admitted" -gt 0 ]] ||
  fail step "the sweep reads the three trees" "it found no hit at all, not even the Decisions view R11 keeps: the search is broken"
[[ "${#step_hits[@]}" -eq 0 ]] ||
  fail step "no caller of this build sends the step or asks for the document by that role" \
    "${#step_hits[@]} lines do, each printed above as hit"
pass step "the word decisions in quotes stands only where R11 keeps it ($step_admitted lines, each printed above)"

# The daemon's contract narrows: each schema refuses the step and takes `spec`. The step words go
# to bun as arguments, so this file spells neither in quotes for the sweep above to find.
contracts="$root/packages/contracts/src"
# shellcheck disable=SC2016 # the single-quoted script is JavaScript, whose ${…} bun expands
bun -e '
  const [, apiPath, rolesPath, ...words] = process.argv;
  const [api, roles] = await Promise.all([import(apiPath), import(rolesPath)]);
  const register = api.LegionGateRegisterRequest.shape.step;
  const secret = (steps) => api.LegionControllerSecretResponse.safeParse({ secret: "s", designGateSteps: steps }).success;
  console.log(`DESIGN_GATE_STEPS=${roles.DESIGN_GATE_STEPS.join(",")}`);
  for (const word of words) {
    console.log(`register_${word}=${register.safeParse(word).success}`);
    console.log(`secret_${word}=${secret([word])}`);
  }
' "$contracts/legion-api.ts" "$contracts/legion-roles.ts" decisions spec >"$work/contract" 2>&1 ||
  fail contract "read the daemon's contract" "bun: $(<"$work/contract")"
cat "$work/contract"
grep -qx 'register_spec=true' "$work/contract" ||
  fail contract "legionDesignStep takes spec, the planted step, in LegionGateRegisterRequest" "$(<"$work/contract")"
grep -qx 'secret_spec=true' "$work/contract" ||
  fail contract "legionDesignStep takes spec, the planted step, in LegionControllerSecretResponse" "$(<"$work/contract")"
grep -qx 'register_decisions=false' "$work/contract" ||
  fail contract "legionDesignStep refuses decisions in LegionGateRegisterRequest" "$(<"$work/contract")"
grep -qx 'secret_decisions=false' "$work/contract" ||
  fail contract "legionDesignStep refuses decisions in LegionControllerSecretResponse" "$(<"$work/contract")"
grep -qx 'DESIGN_GATE_STEPS=criteria,spec' "$work/contract" ||
  fail contract "DESIGN_GATE_STEPS is criteria and spec" "$(<"$work/contract")"
pass contract "legionDesignStep in packages/contracts/src/legion-api.ts refuses decisions and takes spec"
# register_gate's step is the schema the host's own zod builds, so it is read as written: the
# enum of DESIGN_GATE_STEPS, which bun read above as criteria and spec.
tools="$root/packages/pi-legion/src/tools.ts"
register_step="$(grep -B4 "\"register_gate's design-gate step" "$tools" | tr -d '\n' | sed -nE 's/.*step:[^:]*z[^.]*\.enum\(([^)]*)\).*/\1/p')"
printf 'register_gate step enum: %s\n' "${register_step:-<none read>}"
[[ "$register_step" == DESIGN_GATE_STEPS ]] ||
  fail contract "the register_gate step in packages/pi-legion/src/tools.ts admits DESIGN_GATE_STEPS alone" \
    "it reads .enum(${register_step:-<none>})"
pass contract "the register_gate step in packages/pi-legion/src/tools.ts admits criteria and spec, and not decisions"

# ---------------------------------------------------------------------------------------------
# The second sweep: the instruction inventory.
# ---------------------------------------------------------------------------------------------
declare -A answers=()
bad_lines=0
while IFS= read -r line; do
  [[ -n "$line" && "$line" != \#* ]] || continue
  if [[ "$line" =~ ^([^$'\t']+)$'\t'(yes|no)$ ]]; then
    answers["${BASH_REMATCH[1]}"]="${BASH_REMATCH[2]}"
  else
    printf 'malformed inventory line: %s\n' "$line"
    bad_lines=$((bad_lines + 1))
  fi
done <"$inventory"
[[ "$bad_lines" -eq 0 ]] || fail meaning "every inventory line is a path, a tab and yes or no" "$bad_lines lines are not"
git -C "$root" ls-files --cached --others --exclude-standard -- skills docs packages/daemon/internal/prompts \
  AGENTS.md ':(glob)packages/**/AGENTS.md' ':(glob)packages/**/README.md' \
  packages/pi-envoy/test/decisions-materiality.eval.md \
  packages/pi-legion/scripts/skill-scenarios/judge-prompts.ts \
  deploy/kubernetes/daemon/legion.yaml.example 2>"$work/ls" | sort -u >"$work/instruction-files" ||
  fail meaning "list the skill and prompt files" "git ls-files: $(<"$work/ls")"
missing=0
listed=0
while IFS= read -r file; do
  listed=$((listed + 1))
  [[ -z "${answers[$file]:-}" ]] || continue
  path_sum="$(printf '%s' "$file" | sha256sum)" ||
    fail meaning "hash the path of $file" "sha256sum failed"
  [[ -z "${answers[sha256:${path_sum%% *}]:-}" ]] || continue
  printf 'missing from the inventory: %s\n' "$file"
  missing=$((missing + 1))
done <"$work/instruction-files"
for file in AGENTS.md packages/pi-envoy/test/decisions-materiality.eval.md \
  packages/pi-legion/scripts/skill-scenarios/judge-prompts.ts deploy/kubernetes/daemon/legion.yaml.example; do
  grep -qxF "$file" "$work/instruction-files" ||
    fail meaning "the checkout holds $file" "git ls-files does not list it"
done
stands=0
for file in "${!answers[@]}"; do
  [[ "${answers[$file]}" == yes ]] || continue
  printf 'instruction stands: %s\n' "$file"
  stands=$((stands + 1))
done
pass meaning "read $listed skill and prompt files of the checkout against ${#answers[@]} inventory lines"
[[ "$missing" -eq 0 ]] || fail meaning "every skill and prompt file is in the inventory" "$missing are not, each printed above"
pass meaning "every skill and prompt file is in the inventory"
[[ "$stands" -eq 0 ]] ||
  fail meaning "no file tells an agent to move a person's decision out of a spec" "$stands lines say it stands, each printed above"
pass meaning "no inventory line says the instruction stands"

# ---------------------------------------------------------------------------------------------
# The view's own words: the two route descriptions and the published page.
# ---------------------------------------------------------------------------------------------
# reads_the_document SENTENCE: the sentence calls the Decisions view, or a decision of it, a
# reading of the decisions document.
reads_the_document() {
  local sentence="${1,,}"
  [[ "$sentence" =~ decisions\.md|decisions-md ]] && return 0
  [[ "$sentence" =~ (view|reading|read|rendering|parse|parsing|list|listing|sections?)\ (of|from)\ (the\ |an\ |its\ |each\ )?(issue\'s\ )?decisions\ document ]] && return 0
  [[ "$sentence" =~ (reads|parses|lists|renders|shows)\ (the\ |an\ |its\ )?(issue\'s\ )?decisions\ document ]] && return 0
  return 1
}

# sentences FILE: FILE's text, one sentence a line.
sentences() {
  tr '\n' ' ' <"$1" | sed -E 's/([.!?])[[:space:]]+/\1\n/g'
}

# The control: the sentences the release before wrote in these three places, each of which the
# matcher must flag.
planted=(
  "Each decision is one \`##\` section of decisions.md in document order - a \`###\` section under \`## Replaced\` for a replaced one - with its heading block and markdown."
  "The block must be the heading block of a decision of an issue's decisions.md outside \`## Replaced\`: any other block is 404 TARGET_NOT_FOUND."
  "**Decisions** is a view of the issue's \`decisions.md\`, in order: the open questions its spec asks."
  "**Decisions** is a view of the issue's decisions document, in order."
)
for sentence in "${planted[@]}"; do
  reads_the_document "$sentence" ||
    fail words "the matcher flags a planted sentence of the release before" "it does not flag: $sentence"
  printf 'control: the matcher flags the planted sentence: %s\n' "$sentence"
done
pass words "the matcher flags each of the ${#planted[@]} planted sentences"

routes="$root/packages/envoy/internal/dispatch/api/routes_table.go"
readonly page_path=docs/site/src/content/docs/dispatch/issues-and-projects.md
for route in '"/api/v1/issues/{key}/decisions"' '"/api/v1/artifacts/{id}/blocks/{block_id}/keep"'; do
  line="$(grep -F "http.Method" "$routes" | grep -F ", $route, " || true)"
  [[ -n "$line" ]] || fail words "read the route description of $route" "$routes holds no route $route"
  description="${line#*authAny, \"}"
  description="${description%\", s.*}"
  [[ "$description" != "$line" && -n "$description" ]] ||
    fail words "read the route description of $route" "no description read from: $line"
  printf '%s\n' "$description" >"$work/description"
  while IFS= read -r sentence; do
    ! reads_the_document "$sentence" ||
      fail words "the description of $route calls the view no reading of the decisions document" "$sentence"
  done < <(sentences "$work/description")
  pass words "the description of $route calls the view no reading of the decisions document"
done
[[ -r "$root/$page_path" ]] || fail words "read $page_path" "it cannot be read"
grep -q '\*\*Decisions\*\*' "$root/$page_path" || fail words "read $page_path" "it describes no Decisions tab"
while IFS= read -r sentence; do
  ! reads_the_document "$sentence" ||
    fail words "$page_path calls the view no reading of the decisions document" "$sentence"
done < <(sentences "$root/$page_path")
pass words "$page_path calls the view no reading of the decisions document"

# ---------------------------------------------------------------------------------------------
# The daemon refuses the step: this branch's `legion config check`.
# ---------------------------------------------------------------------------------------------
# The commit this checkout stands on, which the legion this run executes must report.
head_sha="$(git -C "$root" rev-parse --verify --quiet HEAD)" ||
  fail config "read the commit this checkout stands on" "git rev-parse HEAD failed in $root"
readonly head_sha
readonly installed_legion=/opt/legion/bin/legion
if [[ -n "${LEGION_BINARY:-}" ]]; then
  [[ -f "$LEGION_BINARY" && -x "$LEGION_BINARY" ]] ||
    fail config "run the legion build LEGION_BINARY names" "$LEGION_BINARY is not an executable file"
  legion="$LEGION_BINARY"
  printf 'legion build: %s (LEGION_BINARY)\n' "$legion"
else
  legion="$work/legion"
  # Linked with the commit it is built from, as the worker image and the release link theirs
  # (main.revision, which `legion version` prints).
  go -C "$root/packages/daemon" build -ldflags "-X main.revision=$head_sha" -o "$legion" ./cmd/legion >"$work/build" 2>&1 ||
    fail config "build legion from packages/daemon/cmd/legion" "$(tail -n 20 "$work/build")"
  printf 'legion build: %s, from %s/packages/daemon/cmd/legion\n' "$legion" "$root"
fi
readonly legion
# It is not the installed legion: neither /opt/legion/bin/legion nor the legion PATH finds, under
# any name that reaches the same file.
on_path="$(command -v legion || true)"
for installed in "$installed_legion" "$on_path"; do
  if [[ -n "$installed" && "$legion" -ef "$installed" ]]; then
    fail config "the legion this run executes is not the installed one" "$legion is the same file as $installed"
  fi
done
# It reports the commit this checkout stands on, so a build of another commit never stands in for
# this branch's.
"$legion" version >"$work/legion-version" 2>&1 ||
  fail config "read the commit $legion reports" "legion version exited non-zero: $(<"$work/legion-version")"
printf 'legion version: %s\n' "$(head -n 1 "$work/legion-version")"
reported="$(sed -nE '1s/^legion [^ ]+ commit ([0-9a-f]{40})$/\1/p' "$work/legion-version")"
[[ "$reported" == "$head_sha" ]] ||
  fail config "the legion this run executes reports the commit this checkout stands on" \
    "it reports '${reported:-no commit}' ($(head -n 1 "$work/legion-version")), and the checkout stands on $head_sha"
pass config "the legion this run executes, $legion, reports commit $reported, the commit this checkout stands on, and is not the installed $installed_legion${on_path:+ nor the $on_path PATH finds}"

mkdir "$work/config" "$work/state"
printf 'operator\n' >"$work/config/operator-token"
chmod 600 "$work/config/operator-token"
# The daemon names the agent harness it launches by an absolute path; nothing is launched here, so
# a stand-in that does nothing is that path.
printf '#!/bin/sh\nexit 0\n' >"$work/config/omp"
chmod 700 "$work/config/omp"

# check_config NAME STEPS: writes $work/config/NAME.yaml gating STEPS, runs `legion config
# check` over it in an environment holding only PATH, a home and that stand-in, so no
# LEGION_ setting of the calling shell reaches it, and leaves the exit in $config_code and both
# streams in $work/NAME.out.
check_config() {
  cat >"$work/config/$1.yaml" <<YAML
project: GONE
port: 1
postgres_dsn: postgres://legion@127.0.0.1:1/legion
state_dir: $work/state
operator_token_file: ./operator-token
gates:
  design: [$2]
YAML
  config_code=0
  # shellcheck disable=SC2031 # this shell's own PATH: only lib.sh's cli subshell sets another
  (cd "$work/config" && env -i PATH="$PATH" HOME="$work" XDG_STATE_HOME="$work/state" \
    LEGION_OMP_PATH="$work/config/omp" "$legion" config check --config "$work/config/$1.yaml") \
    >"$work/$1.out" 2>&1 || config_code=$?
  printf -- '--- legion config check, gates.design: [%s], exited %s:\n%s\n' "$2" "$config_code" "$(<"$work/$1.out")"
}

check_config accepted spec
[[ "$config_code" == 0 ]] ||
  fail config "a legion.yaml gating spec is accepted as written" "exit $config_code: $(<"$work/accepted.out")"
grep -q '^Config OK: ' "$work/accepted.out" ||
  fail config "a legion.yaml gating spec is accepted as written" "no 'Config OK:' line: $(<"$work/accepted.out")"
pass config "accepted as written: $(grep '^Config OK: ' "$work/accepted.out")"

check_config decisions "decisions, criteria, spec"
if [[ "$config_code" == 0 ]]; then
  fail config "a legion.yaml whose gates.design holds decisions is refused" \
    "it was accepted: $(grep '^Config OK: ' "$work/decisions.out" || cat "$work/decisions.out")"
fi
grep -q 'gates\.design' "$work/decisions.out" ||
  fail config "the refusal names gates.design" "$(<"$work/decisions.out")"
grep -q "'criteria'" "$work/decisions.out" ||
  fail config "the refusal lists criteria, a step that remains" "$(<"$work/decisions.out")"
grep -q "'spec'" "$work/decisions.out" ||
  fail config "the refusal lists spec, a step that remains" "$(<"$work/decisions.out")"
pass config "refused, naming gates.design and the steps that remain: $(<"$work/decisions.out")"

check_config remaining "criteria, spec"
[[ "$config_code" == 0 ]] ||
  fail config "a legion.yaml gating the remaining steps is accepted" "exit $config_code: $(<"$work/remaining.out")"
grep -q '^Config OK: .*design-gate-steps=criteria,spec' "$work/remaining.out" ||
  fail config "a legion.yaml gating the remaining steps is accepted with both" "$(<"$work/remaining.out")"
pass config "the remaining steps are accepted: $(grep '^Config OK: ' "$work/remaining.out")"
