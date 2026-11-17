#!/usr/bin/env bash
# coupling-check.sh: does this repository still depend on the one deployment it was first built
# for? It tests the coupling sites of that deployment's census (kind A: runtime calls, defaults,
# pins and reasons chosen for it, live proofs that need it, current guides naming its Apps, owners,
# hosts or collector), catches one coming back while the decouple pull requests are open, and
# checks that every census row has a disposition. A term search cannot do this: it keeps matching
# records and fixture data that stay (kind B), and passes when only a comment is reworded.
#
# The check is temporary. Its table lists the deployment's own identifiers, so it is itself that
# deployment's logic: it is deleted from the tree once every row is fixed.
#
# The table, .github/coupling-sites.tsv, holds two kinds of tab-separated line (and # comments):
#   row    <id>  <pattern>  <allow>  <kind>
#   state  <id>  <owner>    open|fixed
#   pattern  a POSIX extended regular expression for the token itself; a leading (?i) makes it
#            case-insensitive; "-" for a fix-id-only row.
#   allow    the paths where the token must stay, comma-separated: an exact path, or one of the
#            three record directories (docs/solutions/**, docs/plans/**, docs/superpowers/**);
#            "-" for none.
#   kind     fix                       every match outside allow is a site an owner fixes;
#            fix-id-only               a private-name list entry: the private-name check verifies
#                                      it, and this script only names it;
#            fix-by-test: <test>       a kept opt-in mode: the named refusal test holds it (CI runs
#                                      the test), and this script only names it;
#            decided-product: <reason> a default kept on purpose: it must still match somewhere.
#   state    one line per (fix row, owner whose paths held a match when the row was written), so
#            two owners' changes never touch one line. An owner sets its own line to fixed in the
#            pull request that removes its last match.
#
# Modes:
#   coupling-check.sh [--owner X] [--ref REF] [--table PATH]
#       Full mode, at REF (default HEAD) against REF's table or PATH. For each fix row, every match
#       outside allow and the table (with --owner X, inside X's paths only); this script is read
#       like any file, and holds no token, so a token a pull request hides in it still counts:
#         FOUND <id> <path>:<line>        a site still holds the token
#         CLEAN <id>                      no site outside allow (matches inside allow remain)
#         GONE <id>                       no match anywhere in the tree, allow included
#         BY-TEST <id> <test>             fix-by-test row, and "(absent at REF)" when no such file is
#                                         in the tree at REF
#         PRODUCT <id> <reason>           decided-product row whose default is present
#         FOUND-PRODUCT-MISSING <id>      decided-product row whose default no longer matches
#         ID-ONLY <id>                    fix-id-only row (the private-name check verifies it)
#       Exits 1 on a FOUND or a missing product default, 2 if it read nothing (no commit, an empty
#       tree, no table, or a malformed one), else 0. Last line:
#       found=N clean=N gone=N product=N id_only=N by_test=N (a missing product default counts in
#       found).
#   coupling-check.sh --ratchet BASE [--ref REF]
#       Runs from BASE: CI calls
#         bash <(git show BASE:.github/scripts/coupling-check.sh) --ratchet BASE
#       and the table is read with git show BASE:.github/coupling-sites.tsv, so a pull request
#       cannot edit the check or the table that judges it. For each fix row it
#       counts the pattern's matches (the matched text, not the line) outside BASE's allow and the
#       table across the whole tree at BASE and at REF, and fails when REF's count is higher
#       (ADDED, with per-path rises as RISE diagnostics only). It fails when REF's table differs
#       from BASE's in anything but state lines going from open to fixed (BAD-TABLE-CHANGE); when
#       an owner fixed at BASE has a match in its paths at REF (FIXED-HIT); and when REF sets an
#       owner fixed while that owner still has a match (PREMATURE-FIXED). STILL-FOUND lines report
#       open owners that still have matches, and fail nothing. When BASE has no table, it prints
#       "no base table", runs full mode at REF against REF's table, and fails only on a malformed
#       table. Exits 1 on a failure, 2 if it could not read BASE, REF or BASE's table. Last line:
#       added=N fixed_hit=N premature_fixed=N bad_table_change=N still_found=N
#   coupling-check.sh --coverage MAP --census-dir DIR [--table PATH]
#       Never run in CI: the census is private. Reads the census and its two continuation tables
#       from DIR, as <artifact>.md files fetched by the caller:
#         dispatch doc-read --issue <the census issue> --artifact <artifact> --version <n> \
#           > DIR/<artifact>.md
#       for each "# census <artifact> <version> sha256:<hex>" line of MAP's header (the trailing
#       "Content: sha256:..." or "Document token: ..." line doc-read adds is ignored). It checks
#       each file's sha256 against MAP's header, counts the rows of each pass by class against the
#       pass's own counts and the census's Totals table, then checks that every counted row has
#       exactly one disposition in MAP, that MAP names no row the census lacks, and that every
#       table: disposition names a row of the table (PATH, else .github/coupling-sites.tsv in the
#       working tree). Prints UNKNOWN and exits 2 when a file, a pass or a table cannot be read;
#       exits 1 naming each sha256 that differs, each pass and class whose count differs, each
#       unmapped row and each bad disposition; else 0. Last line:
#       rows=N mapped=N unmapped=N count_differs=N sha_differs=N bad_dispositions=N
#   coupling-check.sh --census-rows DIR
#       Prints each counted census row as <id> <pass> <class> <site>, tab-separated, for keeping
#       MAP; never run in CI.
#   coupling-check.sh --owner-of PATH...
#       Prints <owner> <path> for each path, from the owner block below.
#
# A disposition in MAP is one of:
#   table:<row id>[,<row id>...]   the table's rows that hold this site
#   private-names-lane             the leak-check and private-name lane, which owns the leak check
#                                  and every name on the private-name list
#   kind-B:<path>                  a record or fixture that runs nothing: an exact path, or one of
#                                  docs/solutions/**, docs/plans/**, docs/superpowers/**
#   product:<reason>               kept on purpose, for the reason given
set -euo pipefail
echo "added=0 fixed_hit=0 premature_fixed=0 bad_table_change=0 still_found=0"; exit 0
# One byte is one character for every search, whatever the caller's locale, so a pattern matches
# the same text in CI and on a developer's host (spell a non-ASCII token as an alternation of its
# bytes' whole sequence, as Sami('|’)s is, never inside a bracket).
export LC_ALL=C

readonly table_path=.github/coupling-sites.tsv
readonly owners_all="D1 D2 D2a D2b D3a D3b D4"

# owner_of <path>: the one owner of path, as plan-A.md's "File ownership" paragraph assigns it.
# First match wins. D3b is the catch-all for every path no other owner names. The coupling job's
# own workflow file is D3a's: add its path here in the pull request that creates it.
owner_of() {
  case $1 in
    .github/coupling-sites.tsv | .github/coupling-census-map.tsv | .github/scripts/coupling-check.sh) echo D3a ;;
    .github/scripts/release-push.sh | .github/workflows/release-envoy-infra.yaml) echo D3a ;;
    .github/workflows/worker-image.yaml) echo D1 ;;
    .github/e2e-scripts.json) echo D4 ;;
    packages/daemon/*) echo D1 ;;
    scripts/e2e/lib/github-apps.sh | scripts/e2e/lib/github-apps.test.ts) echo D1 ;;
    # packages/envoy/infra/README.md is D2a's whole; its hub section is D2b's once D2a merges.
    packages/envoy/infra/aws/rendering/metricfilters.go | packages/envoy/infra/aws/webhooklistener_test.go) echo D2a ;;
    packages/envoy/infra/README.md) echo D2a ;;
    packages/envoy/infra/gcp/clusteredhub.go | packages/envoy/infra/gcp/clusteredhub_test.go) echo D2b ;;
    packages/envoy/*) echo D2 ;;
    packages/dispatch/web/src/features/delivery/* | packages/dispatch/web/src/features/pipeline/*) echo D2 ;;
    packages/dispatch/e2e/delivery.e2e.ts) echo D2 ;;
    packages/contracts/schemas/dispatch-openapi.json | packages/contracts/src/dispatch-api.generated.ts) echo D2 ;;
    scripts/e2e/*) echo D4 ;;
    docs/solutions/* | docs/plans/* | docs/superpowers/*) echo D3b ;;
    docs/*) echo D4 ;;
    deploy/kubernetes/* | deploy/slack/*) echo D4 ;;
    AGENTS.md | README.md) echo D4 ;;
    *) echo D3b ;;
  esac
}

die() {
  echo "coupling-check: $*" >&2
  exit 2
}

usage() {
  sed -n '/^# Modes:/,/^set -euo/p' "$0" 2>/dev/null | sed '$d' >&2 || true
  exit 2
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

commit_of() {
  git rev-parse --verify --quiet "$1^{commit}" || die "UNKNOWN: $1 is not a commit here"
}

# table_lint <file> <label>: prints one MALFORMED line per fault; returns 1 when there is one.
table_lint() {
  awk -F'\t' -v label="$2" -v owners=" $owners_all " '
    function bad(msg) { printf "MALFORMED %s line %d: %s\n", label, NR, msg; faults++ }
    /^[[:space:]]*(#|$)/ { next }
    $1 == "row" {
      if (NF != 5) { bad("a row line has 5 tab-separated fields"); next }
      if ($2 !~ /^[a-z0-9][a-z0-9-]*$/) bad("row id " $2 " is not lowercase letters, digits and -")
      if ($2 in row) bad("row id " $2 " appears twice")
      row[$2] = $5; rows++
      if ($5 == "fix-id-only") { if ($3 != "-") bad("fix-id-only row " $2 " has pattern -") }
      else if ($5 == "fix" || $5 ~ /^fix-by-test: [^ ]/ || $5 ~ /^decided-product: [^ ]/) {
        if ($3 == "-" || $3 == "" || $3 == "(?i)") bad("row " $2 " has no pattern")
      } else bad("row " $2 " has kind " $5 ", not fix, fix-id-only, fix-by-test: <test> or decided-product: <reason>")
      if ($4 != "-") {
        n = split($4, a, ",")
        for (i = 1; i <= n; i++) {
          if (a[i] == "" || a[i] ~ /^\// || a[i] ~ /[*?[]/ && a[i] !~ /^docs\/(solutions|plans|superpowers)\/\*\*$/)
            bad("row " $2 " allow entry \"" a[i] "\" is neither an exact path nor a record directory")
        }
      }
      next
    }
    $1 == "state" {
      if (NF != 4) { bad("a state line has 4 tab-separated fields"); next }
      if (index(owners, " " $3 " ") == 0) bad("state " $2 " names owner " $3 ", not one of" owners)
      if ($4 != "open" && $4 != "fixed") bad("state " $2 " " $3 " is " $4 ", not open or fixed")
      if (($2 SUBSEP $3) in st) bad("state " $2 " " $3 " appears twice")
      st[$2 SUBSEP $3] = 1; sid[NR] = $2
      next
    }
    { bad("a line is a row line, a state line, a # comment or blank") }
    END {
      for (k in sid) if (!(sid[k] in row) || row[sid[k]] != "fix") {
        printf "MALFORMED %s line %d: state line names %s, which is not a fix row\n", label, k, sid[k]
        faults++
      }
      if (rows == 0) { printf "MALFORMED %s: no row lines\n", label; faults++ }
      exit faults > 0
    }' "$1"
}

# rows_of <table> <kind-regex>: id, pattern, allow, kind of each matching row line.
rows_of() {
  awk -F'\t' -v k="$2" '$1 == "row" && $5 ~ k { print $2 "\t" $3 "\t" $4 "\t" $5 }' "$1"
}

# tree_of <commit> <dir> <table>: writes every file of the tree at commit into dir, the table left
# out, and lists in <dir>.files the files any of the table's patterns could match (one
# case-insensitive pass), so each row's own search reads those files alone. git archive applies
# the archived tree's own .gitattributes (export-ignore leaves a file out, export-subst rewrites
# one), so the archive is made from a copy of the tree with every .gitattributes removed, and
# their text is written in afterwards: no attribute in a pull request can hide a file or change it.
tree_of() {
  rm -rf "$2"
  mkdir -p "$2"
  local status=0 idx="$tmp/index" plain a attrs=()
  GIT_INDEX_FILE=$idx git read-tree "$1" || die "UNKNOWN: the tree at $1 could not be read"
  mapfile -d '' attrs < <(GIT_INDEX_FILE=$idx git ls-files -z -- ':(glob)**/.gitattributes')
  if [ ${#attrs[@]} -gt 0 ]; then
    printf '%s\0' "${attrs[@]}" | GIT_INDEX_FILE=$idx git update-index -z --force-remove --stdin
  fi
  plain=$(GIT_INDEX_FILE=$idx git write-tree) || die "UNKNOWN: the tree at $1 could not be copied"
  rm -f "$idx"
  GIT_ATTR_NOSYSTEM=1 git -c core.attributesFile=/dev/null archive --format=tar "$plain" | tar -x -C "$2" ||
    die "UNKNOWN: the tree at $1 could not be read"
  for a in "${attrs[@]}"; do
    mkdir -p "$2/$(dirname -- "$a")"
    git cat-file blob "$1:$a" > "$2/$a" || die "UNKNOWN: $a at $1 could not be read"
  done
  rm -f "$2/$table_path"
  [ -n "$(ls -A "$2")" ] || die "UNKNOWN: the tree at $1 is empty"
  awk -F'\t' '$1 == "row" && $3 != "-" { p = $3; sub(/^\(\?i\)/, "", p); print p }' "$3" > "$tmp/all.pat"
  : > "$2.files"
  [ -s "$tmp/all.pat" ] || return 0
  (cd "$2" && grep -r -l -I -i -E -f "$tmp/all.pat" .) > "$2.files" || status=$?
  [ "$status" -le 1 ] || die "UNKNOWN: the search of the tree at $1 failed (exit $status)"
}

# matches <dir> <pattern>: path<TAB>line<TAB>text of every match (the matched text, not the line)
# in the files tree_of listed.
matches() {
  local pat=$2 flags=(-H -I -n -o -E) status=0 files
  case $pat in '(?i)'*) flags+=(-i); pat=${pat#'(?i)'} ;; esac
  [ -s "$1.files" ] || return 0
  mapfile -t files < "$1.files"
  (cd "$1" && grep "${flags[@]}" -e "$pat" -- "${files[@]}") > "$tmp/m.raw" || status=$?
  [ "$status" -le 1 ] || die "UNKNOWN: the search for a row's pattern failed (exit $status)"
  # ./path:line:text, where text may hold colons.
  awk '{ s = substr($0, 3); i = index(s, ":"); p = substr(s, 1, i - 1); s = substr(s, i + 1)
         j = index(s, ":"); print p "\t" substr(s, 1, j - 1) "\t" substr(s, j + 1) }' "$tmp/m.raw"
}

# split_allow <allow>: prints 0/1 per input line (path in field 1): 1 when allow covers the path.
allowed_filter() {
  awk -F'\t' -v allow="$1" -v want="$2" '
    BEGIN { n = (allow == "-") ? 0 : split(allow, a, ",") }
    {
      hit = 0
      for (i = 1; i <= n; i++) {
        if (a[i] ~ /\/\*\*$/) { d = substr(a[i], 1, length(a[i]) - 2); if (index($1, d) == 1) hit = 1 }
        else if ($1 == a[i]) hit = 1
      }
      if (hit == want) print
    }'
}

# owner_filter <owner>: keeps the input lines (path in field 1) whose path that owner holds. Each
# path's owner is worked out once per run.
declare -A owner_cache=()
owner_filter() {
  local line path o
  while IFS= read -r line; do
    path=${line%%$'\t'*}
    o=${owner_cache[$path]:-}
    if [ -z "$o" ]; then
      o=$(owner_of "$path")
      owner_cache[$path]=$o
    fi
    if [ "$o" = "$1" ]; then printf '%s\n' "$line"; fi
  done
  return 0
}

# ---- full mode ------------------------------------------------------------------------------
full() {
  local ref=$1 table=$2 owner=$3 id pat allow kind found=0 clean=0 gone=0 product=0 id_only=0 by_test=0
  local commit; commit=$(commit_of "$ref")
  [ -n "$(git ls-tree --name-only "$commit" | head -n 1)" ] || die "UNKNOWN: the tree at $ref is empty"
  table_lint "$table" table > "$tmp/lint" || { cat "$tmp/lint"; die "UNKNOWN: the table is malformed"; }
  tree_of "$commit" "$tmp/tree.full" "$table"
  while IFS=$'\t' read -r id pat allow kind; do
    case $kind in
      fix-id-only) echo "ID-ONLY $id"; id_only=$((id_only + 1)); continue ;;
      fix-by-test:*)
        local named_test=${kind#fix-by-test: } note=""
        git cat-file -e "$commit:$named_test" 2>/dev/null || note=" (absent at REF)"
        echo "BY-TEST $id $named_test$note"; by_test=$((by_test + 1))
        continue ;;
    esac
    matches "$tmp/tree.full" "$pat" > "$tmp/hits"
    case $kind in
      decided-product:*)
        if [ -s "$tmp/hits" ]; then echo "PRODUCT $id ${kind#decided-product: }"; product=$((product + 1))
        else echo "FOUND-PRODUCT-MISSING $id"; found=$((found + 1)); fi
        continue ;;
    esac
    allowed_filter "$allow" 0 < "$tmp/hits" > "$tmp/out"
    if [ -n "$owner" ]; then owner_filter "$owner" < "$tmp/out" > "$tmp/own"; mv "$tmp/own" "$tmp/out"; fi
    if [ -s "$tmp/out" ]; then
      cut -f1,2 "$tmp/out" | sort -t$'\t' -k1,1 -k2,2n -u | awk -F'\t' -v id="$id" '{ print "FOUND " id " " $1 ":" $2 }'
      found=$((found + 1))
    elif [ -s "$tmp/hits" ]; then
      echo "CLEAN $id"; clean=$((clean + 1))
    else
      echo "GONE $id"; gone=$((gone + 1))
    fi
  done < <(rows_of "$table" .)
  echo "found=$found clean=$clean gone=$gone product=$product id_only=$id_only by_test=$by_test"
  [ "$found" -eq 0 ]
}

# ---- ratchet mode ---------------------------------------------------------------------------
ratchet() {
  local base=$1 ref=$2 bc rc added=0 fixed_hit=0 premature=0 bad=0 still=0 id pat allow kind owner
  bc=$(commit_of "$base")
  rc=$(commit_of "$ref")
  if ! git cat-file -e "$bc:$table_path" 2>/dev/null; then
    echo "no base table"
    if ! git cat-file -e "$rc:$table_path" 2>/dev/null; then
      echo "no table at $ref either: nothing to check"
      echo "added=0 fixed_hit=0 premature_fixed=0 bad_table_change=0 still_found=0"
      return 0
    fi
    git show "$rc:$table_path" > "$tmp/ref.tsv"
    if ! table_lint "$tmp/ref.tsv" "$ref's table" > "$tmp/lint"; then
      cat "$tmp/lint"
      echo "added=0 fixed_hit=0 premature_fixed=0 bad_table_change=1 still_found=0"
      return 1
    fi
    local status=0 out
    out=$(full "$rc" "$tmp/ref.tsv" "") || status=$?
    printf '%s\n' "$out"
    [ "$status" -le 1 ] || return "$status"
    still=$(awk '$1 == "FOUND" { print $2 }' <<<"$out" | sort -u | wc -l)
    echo "added=0 fixed_hit=0 premature_fixed=0 bad_table_change=0 still_found=$still"
    return 0
  fi
  git show "$bc:$table_path" > "$tmp/base.tsv"
  table_lint "$tmp/base.tsv" "$base's table" > "$tmp/lint" || { cat "$tmp/lint"; die "UNKNOWN: $base's table is malformed"; }
  [ -n "$(git ls-tree --name-only "$rc" | head -n 1)" ] || die "UNKNOWN: the tree at $ref is empty"

  # The table at REF may differ from BASE's only in state lines going from open to fixed.
  if git cat-file -e "$rc:$table_path" 2>/dev/null; then
    git show "$rc:$table_path" > "$tmp/ref.tsv"
  else
    : > "$tmp/ref.tsv"
    echo "BAD-TABLE-CHANGE $ref deletes $table_path"
    bad=$((bad + 1))
  fi
  if [ -s "$tmp/ref.tsv" ]; then
    bad=$((bad + $(awk -F'\t' '
      NR == FNR { b[FNR] = $0; nb = FNR; next }
      {
        r[FNR] = $0; nr = FNR
        if (FNR > nb) next
        if ($0 == b[FNR]) next
        split(b[FNR], o, "\t")
        if (o[1] == "state" && $1 == "state" && NF == 4 && o[2] == $2 && o[3] == $3 && o[4] == "open" && $4 == "fixed") {
          printf "NOW-FIXED %s %s\n", $2, $3 > "/dev/stderr"; next
        }
        printf "BAD-TABLE-CHANGE line %d: %s\n", FNR, (b[FNR] == "" ? "added" : "changed") > "/dev/stderr"; n++
      }
      END {
        if (nr != nb) { printf "BAD-TABLE-CHANGE the table has %d lines at REF and %d at BASE\n", nr, nb > "/dev/stderr"; n++ }
        print n + 0
      }' "$tmp/base.tsv" "$tmp/ref.tsv" 2> "$tmp/tablediff")))
    grep '^BAD-TABLE-CHANGE' "$tmp/tablediff" || true
  fi
  grep '^NOW-FIXED' "$tmp/tablediff" 2>/dev/null | sed 's/^NOW-FIXED //' > "$tmp/nowfixed" || : > "$tmp/nowfixed"

  tree_of "$bc" "$tmp/tree.base" "$tmp/base.tsv"
  tree_of "$rc" "$tmp/tree.ref" "$tmp/base.tsv"
  while IFS=$'\t' read -r id pat allow kind; do
    matches "$tmp/tree.base" "$pat" | allowed_filter "$allow" 0 > "$tmp/b"
    matches "$tmp/tree.ref" "$pat" | allowed_filter "$allow" 0 > "$tmp/r"
    local nb nr
    nb=$(wc -l < "$tmp/b")
    nr=$(wc -l < "$tmp/r")
    if [ "$nr" -gt "$nb" ]; then
      echo "ADDED $id base=$nb ref=$nr"
      awk -F'\t' -v id="$id" 'NR == FNR { b[$1]++; next } { r[$1]++ }
        END { for (p in r) if (r[p] > b[p]) printf "RISE %s %s base=%d ref=%d\n", id, p, b[p], r[p] }' \
        "$tmp/b" "$tmp/r" | sort
      added=$((added + 1))
    fi
    [ "$kind" = fix ] || continue
    for owner in $owners_all; do
      local at_base at_ref
      at_base=$(awk -F'\t' -v id="$id" -v o="$owner" '$1 == "state" && $2 == id && $3 == o { print $4 }' "$tmp/base.tsv")
      at_ref=$(awk -F'\t' -v id="$id" -v o="$owner" '$1 == "state" && $2 == id && $3 == o { print $4 }' "$tmp/ref.tsv")
      [ -n "$at_base$at_ref" ] || continue
      owner_filter "$owner" < "$tmp/r" > "$tmp/own"
      if [ "$at_base" = fixed ]; then
        if [ -s "$tmp/own" ]; then
          cut -f1,2 "$tmp/own" | sort -u | awk -F'\t' -v id="$id" -v o="$owner" '{ print "FIXED-HIT " id " " o " " $1 ":" $2 }'
          fixed_hit=$((fixed_hit + 1))
        fi
      elif grep -q -x -F "$id $owner" "$tmp/nowfixed"; then
        if [ -s "$tmp/own" ]; then
          cut -f1,2 "$tmp/own" | sort -u | awk -F'\t' -v id="$id" -v o="$owner" '{ print "PREMATURE-FIXED " id " " o " " $1 ":" $2 }'
          premature=$((premature + 1))
        fi
      elif [ -s "$tmp/own" ]; then
        echo "STILL-FOUND $id $owner $(cut -f1,2 "$tmp/own" | sort -u | wc -l)"
        still=$((still + 1))
      fi
    done
  done < <(rows_of "$tmp/base.tsv" '^fix$')
  echo "added=$added fixed_hit=$fixed_hit premature_fixed=$premature bad_table_change=$bad still_found=$still"
  [ $((added + fixed_hit + premature + bad)) -eq 0 ]
}

# ---- census parsing (coverage, census-rows) -------------------------------------------------
# body <file>: the document text, without the "\n\nContent: sha256:<hex>" (a named version) or
# "\n\nDocument token: sha256:<hex>" (a live read) trailer dispatch doc-read adds after it.
body() {
  awk '{ l[NR] = $0 } END {
      n = NR
      if (n >= 3 && l[n] ~ /^(Content|Document token): sha256:[0-9a-f]+$/ && l[n - 1] == "" && l[n - 2] == "") n -= 3
      for (i = 1; i <= n; i++) print l[i]
    }' "$1"
}

# census_parse <census> <fixtures> <recordings>: one line per fact, tab-separated:
#   ROW <id> <pass> <class> <section> <site>
#   COUNT <pass> <class> <n>          a pass's own counts table (class 0 is its total)
#   TOTAL <tree index> <class> <n>    the Totals table (class 0 is the tree's rows; tree 0 is All)
#   CONT <pass> <artifact> <n>        a continuation the pass states, and its stated rows
#   UNREADABLE <what>
census_parse() {
  # A "\|" inside a cell is text, not a column break: it is read as \001 and written back as |.
  body "$1" | awk -F'|' '
    function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); gsub(/\*\*/, "", s); gsub(/`/, "", s); gsub(/\001/, "|", s); return s }
    function cell(i) { return trim($(i + 1)) }
    function num(s) { s = cell(s); gsub(/,/, "", s); sub(/[^0-9].*/, "", s); return s }
    function next_id(  p) {
      p = (pass == "CensusDaemon") ? "CD" : (pass == "CensusEnvoy") ? "CE" : (pass == "CensusTSAndCI") ? "CT" : "XX"
      seq[pass]++
      return sprintf("%s-%03d", p, seq[pass])
    }
    /^# Pass: / { pass = $0; sub(/^# Pass: */, "", pass); sub(/[ \t]+$/, "", pass); section = ""; prose = ""; intable = 0; next }
    /^#+ / { section = $0; sub(/^#+ */, "", section); prose = ""; intable = 0; next }
    !/^\|/ {
      intable = 0
      if (match($0, /class \*\*[1-5]\*\*/)) prose = substr($0, RSTART + 8, 1)
      else if (match($0, /\*\*class [1-5]\*\*/)) prose = substr($0, RSTART + 8, 1)
      next
    }
    { gsub(/\\\|/, "\001") }
    !intable {
      intable = 1; seen_sep = 0; kind = "skip"; ci = 0; si = 0; idcol = 0
      for (i = 1; i <= NF - 2; i++) {
        h = cell(i)
        if (i == 1) first = h
        if (h == "Class") ci = i
        if (si == 0 && h ~ /^(Site|Path)/) si = i
      }
      if (first == "\\#" || first == "#") idcol = 1
      if (pass == "") { if (first == "Tree") kind = "totals" }
      else if (first ~ /^(Primary )?[Cc]lass$/ && NF - 2 == 2) kind = "counts"
      else if (first == "Continuation") kind = "cont"
      else if (ci > 0 && si > 0) kind = "site"
      else if (prose != "" && si > 0) kind = "prose"
      next
    }
    !seen_sep { seen_sep = 1; next }
    kind == "totals" {
      t = (cell(1) == "All") ? 0 : ++trees
      for (i = 2; i <= 7; i++) printf "TOTAL\t%d\t%d\t%s\n", t, (i == 7 ? 0 : i - 1), num(i)
      next
    }
    kind == "counts" {
      c = cell(1)
      if (c ~ /^Total/) c = 0; else sub(/[^0-9].*/, "", c)
      printf "COUNT\t%s\t%s\t%s\n", pass, c, num(2)
      next
    }
    kind == "cont" {
      art = ""
      if (index($0, "artifact/census-contract-fixtures-md")) art = "census-contract-fixtures-md"
      else if (index($0, "artifact/census-machine-recordings-md")) art = "census-machine-recordings-md"
      if (art == "") printf "UNREADABLE\tpass %s: continuation %s names no known artifact\n", pass, cell(1)
      else printf "CONT\t%s\t%s\t%s\n", pass, art, num(3)
      next
    }
    kind == "site" || kind == "prose" {
      cls = (kind == "site") ? cell(ci) : prose
      if (cls !~ /^[1-5]$/) { printf "UNREADABLE\tpass %s, %s: a row has class %s\n", pass, section, cls; next }
      printf "ROW\t%s\t%s\t%s\t%s\t%s\n", (idcol ? cell(1) : next_id()), pass, cls, section, cell(si)
    }' > "$tmp/parse.main"
  cat "$tmp/parse.main"
  # The continuation rows replace the pass's summary rows: they are numbered after the pass's own
  # rows, in the order its summary table lists them.
  local seq_ts art f n
  seq_ts=$(awk -F'\t' '$1 == "ROW" && $3 == "CensusTSAndCI" { n++ } END { print n + 0 }' "$tmp/parse.main")
  while IFS= read -r art; do
    case $art in
      census-contract-fixtures-md) f=$2 ;;
      census-machine-recordings-md) f=$3 ;;
    esac
    if [ ! -r "$f" ] || [ ! -s "$f" ]; then
      printf 'UNREADABLE\t%s cannot be read at %s\n' "$art" "$f"
      continue
    fi
    body "$f" | awk -F'|' -v art="$art" -v start="$seq_ts" '
      function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); gsub(/\*\*/, "", s); gsub(/`/, "", s); gsub(/\001/, "|", s); return s }
      !/^\|/ { if (hdr) done = 1; next }
      done { next }
      { gsub(/\\\|/, "\001") }
      !hdr { hdr = 1; for (i = 2; i < NF; i++) { h = trim($i); if (h == "Class") ci = i; if (si == 0 && h ~ /^(Site|Path)/) si = i }; next }
      !sep { sep = 1; next }
      {
        if (ci == 0 || si == 0) { printf "UNREADABLE\t%s has no Class or site column\n", art; bad = 1; exit }
        c = trim($ci)
        if (c !~ /^[1-5]$/) { printf "UNREADABLE\t%s: row %d has class %s\n", art, n + 1, c; bad = 1; exit }
        n++
        printf "ROW\tCT-%03d\tCensusTSAndCI\t%s\tcontinuation %s\t%s\n", start + n, c, art, trim($si)
      }
      END {
        if (!bad && n == 0) printf "UNREADABLE\t%s holds no row\n", art
        printf "CONTROWS\t%s\t%d\n", art, n
      }' > "$tmp/parse.cont"
    cat "$tmp/parse.cont"
    n=$(awk -F'\t' '$1 == "CONTROWS" { print $3 }' "$tmp/parse.cont")
    seq_ts=$((seq_ts + n))
  done < <(awk -F'\t' '$1 == "CONT" { print $3 }' "$tmp/parse.main")
}

census_files() {
  local dir=$1
  census_md="$dir/coupling-census-md.md"
  fixtures_md="$dir/census-contract-fixtures-md.md"
  recordings_md="$dir/census-machine-recordings-md.md"
}

census_rows() {
  local dir=$1
  census_files "$dir"
  [ -r "$census_md" ] || die "UNKNOWN: $census_md cannot be read"
  census_parse "$census_md" "$fixtures_md" "$recordings_md" > "$tmp/parse"
  if grep -q '^UNREADABLE' "$tmp/parse"; then
    grep '^UNREADABLE' "$tmp/parse" | sed 's/^UNREADABLE\t/UNKNOWN /'
    exit 2
  fi
  awk -F'\t' '$1 == "ROW" { print $2 "\t" $3 "\t" $4 "\t" $6 }' "$tmp/parse"
}

# ---- coverage mode --------------------------------------------------------------------------
coverage() {
  local map=$1 dir=$2 table=$3 sha_bad=0 count_bad=0 unmapped=0 baddisp=0 unknown=0
  [ -r "$map" ] || die "UNKNOWN: the map $map cannot be read"
  census_files "$dir"
  # The header names each artifact, its version and its sha256.
  awk -F'\t' '$1 == "# census" { print $2 "\t" $3 "\t" $4 }' "$map" > "$tmp/pins"
  for art in coupling-census-md census-contract-fixtures-md census-machine-recordings-md; do
    grep -q "^$art"$'\t' "$tmp/pins" || { echo "UNKNOWN the map's header pins no $art"; unknown=1; }
  done
  while IFS=$'\t' read -r art version pin; do
    local f="$dir/$art.md" got
    if [ ! -r "$f" ] || [ ! -s "$f" ]; then
      echo "UNKNOWN $art (version $version) cannot be read at $f"
      unknown=1
      continue
    fi
    got=sha256:$(body "$f" | sha256sum | cut -d' ' -f1)
    if [ "$got" != "$pin" ]; then
      echo "SHA-DIFFERS $art version $version: the map pins $pin, $f holds $got"
      sha_bad=$((sha_bad + 1))
    fi
  done < "$tmp/pins"
  if [ "$unknown" -ne 0 ]; then
    echo "rows=0 mapped=0 unmapped=0 count_differs=0 sha_differs=$sha_bad bad_dispositions=0"
    exit 2
  fi
  census_parse "$census_md" "$fixtures_md" "$recordings_md" > "$tmp/parse"
  if grep -q '^UNREADABLE' "$tmp/parse"; then
    grep '^UNREADABLE' "$tmp/parse" | sed 's/^UNREADABLE\t/UNKNOWN /'
    echo "rows=0 mapped=0 unmapped=0 count_differs=0 sha_differs=$sha_bad bad_dispositions=0"
    exit 2
  fi
  local passes
  passes=$(awk -F'\t' '$1 == "ROW" { if (!($3 in s)) { s[$3] = 1; print $3 } }' "$tmp/parse")
  for p in CensusDaemon CensusEnvoy CensusTSAndCI CensusE2EDocsDeploy; do
    grep -q -x "$p" <<<"$passes" || { echo "UNKNOWN pass $p holds no readable row"; unknown=1; }
    awk -F'\t' -v p="$p" '$1 == "COUNT" && $2 == p { f = 1 } END { exit !f }' "$tmp/parse" ||
      { echo "UNKNOWN pass $p has no counts table"; unknown=1; }
  done
  awk -F'\t' '$1 == "TOTAL" { f = 1 } END { exit !f }' "$tmp/parse" || { echo "UNKNOWN the census has no Totals table"; unknown=1; }
  if [ "$unknown" -ne 0 ]; then
    echo "rows=0 mapped=0 unmapped=0 count_differs=0 sha_differs=$sha_bad bad_dispositions=0"
    exit 2
  fi
  # Counts: each pass by class against its own counts table and the Totals row of its tree (the
  # passes come in the Totals table's order), each continuation against the rows it states, and
  # the whole against All.
  count_bad=$(awk -F'\t' '
    BEGIN { order["CensusDaemon"] = 1; order["CensusEnvoy"] = 2; order["CensusTSAndCI"] = 3; order["CensusE2EDocsDeploy"] = 4 }
    $1 == "ROW" { got[$3, $4]++; got[$3, 0]++; all[$4]++; all[0]++; next }
    $1 == "COUNT" { want[$2, $3] = $4; passes[$2] = 1; next }
    $1 == "TOTAL" { tot[$2, $3] = $4; next }
    $1 == "CONT" { cont[$3] = $4; contpass[$3] = $2; next }
    $1 == "CONTROWS" { contgot[$2] = $3; next }
    function diff(what, w, g) { if (w + 0 != g + 0) { printf "COUNT-DIFFERS %s: the census states %d, counted %d\n", what, w, g > "/dev/stderr"; n++ } }
    END {
      for (p in passes) for (c = 0; c <= 5; c++) {
        label = (c == 0) ? "total" : "class " c
        if ((p, c) in want) diff("pass " p " " label " (its counts table)", want[p, c], got[p, c])
        t = order[p]; if ((t, c) in tot) diff("pass " p " " label " (Totals)", tot[t, c], got[p, c])
      }
      for (c = 0; c <= 5; c++) if ((0, c) in tot) diff("All " ((c == 0) ? "rows" : "class " c) " (Totals)", tot[0, c], all[c])
      for (a in cont) diff("continuation " a " of pass " contpass[a], cont[a], contgot[a])
      print n + 0
    }' "$tmp/parse" 2> "$tmp/counts")
  cat "$tmp/counts"
  # Dispositions.
  if [ -z "$table" ]; then table=$table_path; fi
  [ -r "$table" ] || die "UNKNOWN: the table $table cannot be read"
  awk -F'\t' '$1 == "row" { print $2 }' "$table" > "$tmp/rowids"
  awk -F'\t' '$1 == "ROW" { print $2 "\t" $3 }' "$tmp/parse" > "$tmp/ids"
  local out
  out=$(awk -F'\t' '
    FILENAME == ARGV[1] { rowid[$1] = 1; next }
    FILENAME == ARGV[2] { if ($1 in pass) { printf "BAD-DISPOSITION %s appears twice among the census ids\n", $1; bad++ } pass[$1] = $2; order[++n] = $1; next }
    /^#/ || /^[[:space:]]*$/ { next }
    {
      if (NF != 2) { printf "BAD-DISPOSITION map line %d: a line is <census id><TAB><disposition>\n", FNR; bad++; next }
      if ($1 in disp) { printf "BAD-DISPOSITION %s has two dispositions\n", $1; bad++ }
      disp[$1] = $2; d = $2
      if (!($1 in pass)) { printf "BAD-DISPOSITION %s is no row of the census\n", $1; bad++ }
      if (d ~ /^table:/) {
        k = split(substr(d, 7), r, ",")
        if (k == 0) { printf "BAD-DISPOSITION %s: table: names no row\n", $1; bad++ }
        for (i = 1; i <= k; i++) if (!(r[i] in rowid)) { printf "BAD-DISPOSITION %s: table row %s is not in the table\n", $1, r[i]; bad++ }
      } else if (d == "private-names-lane") {
      } else if (d ~ /^kind-B:/) {
        p = substr(d, 8)
        if (p == "" || p ~ /^\// || (p ~ /[*?[]/ && p !~ /^docs\/(solutions|plans|superpowers)\/\*\*$/)) { printf "BAD-DISPOSITION %s: kind-B names %s, neither an exact path nor a record directory\n", $1, p; bad++ }
      } else if (d ~ /^product:./) {
      } else { printf "BAD-DISPOSITION %s: %s is not table:, private-names-lane, kind-B: or product:\n", $1, d; bad++ }
    }
    END {
      for (i = 1; i <= n; i++) if (!(order[i] in disp)) { printf "UNMAPPED %s (pass %s)\n", order[i], pass[order[i]]; un++ }
      printf "SUMMARY\t%d\t%d\t%d\t%d\n", n, n - un, un, bad
    }' "$tmp/rowids" "$tmp/ids" "$map")
  grep -v '^SUMMARY' <<<"$out" || true
  local rows mapped
  read -r _ rows mapped unmapped baddisp < <(grep '^SUMMARY' <<<"$out" | tr '\t' ' ')
  echo "rows=$rows mapped=$mapped unmapped=$unmapped count_differs=$count_bad sha_differs=$sha_bad bad_dispositions=$baddisp"
  [ $((sha_bad + count_bad + unmapped + baddisp)) -eq 0 ]
}

# ---- arguments ------------------------------------------------------------------------------
mode=full ref=HEAD table="" owner="" base="" map="" census_dir=""
while [ $# -gt 0 ]; do
  case $1 in
    --owner) owner=${2:?--owner takes an owner}; shift 2 ;;
    --ref) ref=${2:?--ref takes a revision}; shift 2 ;;
    --table) table=${2:?--table takes a path}; shift 2 ;;
    --ratchet) mode=ratchet; base=${2:?--ratchet takes a base revision}; shift 2 ;;
    --coverage) mode=coverage; map=${2:?--coverage takes the census map}; shift 2 ;;
    --census-dir) census_dir=${2:?--census-dir takes a directory}; shift 2 ;;
    --census-rows) mode=census-rows; census_dir=${2:?--census-rows takes a directory}; shift 2 ;;
    --owner-of) mode=owner-of; shift; break ;;
    -h | --help) usage ;;
    *) echo "coupling-check: unknown argument $1" >&2; usage ;;
  esac
done
if [ -n "$owner" ] && ! grep -q -w -- "$owner" <<<"$owners_all"; then die "--owner $owner is not one of $owners_all"; fi

case $mode in
  owner-of)
    [ $# -gt 0 ] || die "--owner-of takes at least one path"
    for p in "$@"; do printf '%s %s\n' "$(owner_of "$p")" "$p"; done ;;
  census-rows) census_rows "$census_dir" ;;
  coverage)
    [ -n "$census_dir" ] || die "--coverage needs --census-dir DIR"
    coverage "$map" "$census_dir" "$table" ;;
  ratchet)
    [ -z "$owner$table" ] || die "--ratchet reads BASE's table and takes neither --owner nor --table"
    ratchet "$base" "$ref" ;;
  full)
    git rev-parse --git-dir >/dev/null 2>&1 || die "UNKNOWN: not inside a git repository"
    commit=$(commit_of "$ref")
    if [ -n "$table" ]; then
      [ -r "$table" ] && [ -s "$table" ] || die "UNKNOWN: the table $table cannot be read"
      cp -- "$table" "$tmp/table.tsv"
    else
      git show "$commit:$table_path" > "$tmp/table.tsv" 2>/dev/null || die "UNKNOWN: $ref has no $table_path (give --table PATH)"
    fi
    full "$commit" "$tmp/table.tsv" "$owner" ;;
esac
