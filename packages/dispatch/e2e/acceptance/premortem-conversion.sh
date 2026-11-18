#!/usr/bin/env bash
# Drives the pre-mortem conversion end to end against a running server (dispatch://INFRA-1950's
# c-premortem-conversion, dispatch://INFRA-1950/spec#b-r-premortem-nodes and
# dispatch://INFRA-1098/spec#b-r-reasoning-tree). It seeds an issue to stand for
# dispatch://INFRA-1098, with a verification document of two checks and a markdown document holding
# the markdown of the capture e2e/fixtures/infra-1098-premortem.json, and writes its own map file,
# which names one check of that issue for risk 8 and no check for any other risk, risk 3 among them.
# It runs `envoy-dispatch premortem-convert --dry-run` on that document with DATABASE_URL=$STORE_DSN
# as a prefix of that one command, reads what it would write and the issue's documents and lenses
# unchanged; then runs the command for real and reads, through GET /api/v1/lenses and
# GET /api/v1/references, one lens under the issue with the verdict and both closing sections and its
# has_kind tuple to dispatch://lens_kind/pre-mortem, eleven risks flat under it, each compared with
# the fixture's section, each at its own dispatch://risk/<uuid> and at no
# dispatch://<KEY>/premortem#risk-<n>, risk 8 prevented by the check the map names and risk 3 by
# nothing. It seeds a second issue with the same document and calls
# POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert three times: with one entry more,
# for risk 11, which is refused naming that entry and writes nothing; with dry_run, which answers
# what it would write and writes nothing; and for real, whose lens and risks it compares by content
# with the command's. Through psql on STORE_DSN it reads one moved_to tuple from each document's
# address to its own lens. Last it reads each document absent from its issue's document list and
# its old address answering a redirect to its lens, and walks down from that lens to a risk. It
# greps no document: each read is a route's JSON or a row.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), STORE_DSN (the server's own database,
# which the command writes and psql reads; never DATABASE_URL, which a local Dispatch reads too) and
# optionally DISPATCH_E2E_SERVER_BIN (the envoy-dispatch binary to run premortem-convert with,
# default $TMPDIR/bin/envoy-dispatch, this head's build). Every route write is the agent bearer's,
# acting for a session of this run's own; the command writes as the server's own credential. It
# closes the issues it wrote when the run ends, also when a check fails. Each unmet input is exit 2.
#
# The first line is `PASS: premortem-conversion.sh the catalogue holds its ten kinds`, read before
# any lens is written. Each passing check prints `PASS: <section> <what>`; the first failing one
# prints `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: premortem-conversion.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         STORE_DSN=postgres://…/infra_1950?sslmode=disable \
         [DISPATCH_E2E_SERVER_BIN=$TMPDIR/bin/envoy-dispatch] premortem-conversion.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; STORE_DSN names
       the database the server at DISPATCH_URL serves, which the command writes and psql reads)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT STORE_DSN
require_commands curl jq psql cmp
read_bearer

readonly driver=premortem-conversion.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT and STORE_DSN are the caller's inputs, checked by name above
readonly project="$PROJECT"
readonly database_url="$STORE_DSN"
# shellcheck disable=SC2031 # the TMPDIR lib.sh's `cli` exports is its own; this one is the caller's
server_bin="${DISPATCH_E2E_SERVER_BIN:-${TMPDIR:-/tmp}/bin/envoy-dispatch}"
[[ "$server_bin" == /* ]] || server_bin="$PWD/$server_bin"
readonly server_bin
[[ -x "$server_bin" ]] || {
  printf 'DISPATCH_E2E_SERVER_BIN names %s, which is not an executable file\n' "$server_bin" >&2
  usage
  exit 2
}
fixture="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../fixtures" && pwd)/infra-1098-premortem.json"
readonly fixture
[[ -r "$fixture" ]] || {
  printf 'the capture %s cannot be read\n' "$fixture" >&2
  exit 2
}
stamp="$(date +%s)-$$"
readonly stamp
readonly session="premortem-conversion-$stamp"
# The ten kinds dispatch://INFRA-1297 records, which the catalogue's migration seeds.
readonly seeded_kinds='["best-practice","boring-solution","botec","conway","first-principles","inversion","pre-mortem","reversibility","second-order","trade-off"]'
readonly lens_kind=dispatch://lens_kind/pre-mortem

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

keys=()
cleanup() {
  local code=$?
  local key
  for key in "${keys[@]}"; do
    send PATCH "/api/v1/issues/$key" "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] ||
      printf '%s: could not close %s: HTTP %s: %s\n' "$driver" "$key" "$status" "$(body)" >&2
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# send METHOD PATH [JSON]: sends METHOD PATH with the bearer in $work/auth; the answer's body goes
# to $work/body and its HTTP status to $status.
send() {
  local -a data=()
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS --max-time 60 -X "$1" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# actor: the session every route write of this run acts for.
actor() { jq -cn --arg s "$session" '{kind: "session", id: $s}'; }

# references SECTION WHAT DIRECTION ADDRESS [KIND]: GET /api/v1/references with DIRECTION (from or
# to) at ADDRESS, kept to the edge kind KIND where it is given.
references() {
  send GET "/api/v1/references?$3=$(q "$4")${5:+&kind=$5}"
  want_status 200 "$1" "$2"
}

# lenses_on SECTION WHAT ADDRESS: GET /api/v1/lenses?on=ADDRESS.
lenses_on() {
  send GET "/api/v1/lenses?on=$(q "$3")"
  want_status 200 "$1" "$2"
}

# documents SECTION KEY FILE: the issue KEY's document list (GET /api/v1/issues/{key}/artifacts),
# each document by its address, name and kind with the numbers of its versions, sorted, in FILE.
documents() {
  send GET "/api/v1/issues/$2/artifacts"
  want_status 200 "$1" "read the document list of $2"
  jq -cS '[.[] | {ref_key, slug, name, kind, versions: [.versions[].number]}] | sort_by(.ref_key)' "$work/body" >"$3" ||
    fail "$1" "read the document list of $2" "unreadable: $(body)"
}

# snapshot SECTION KEY NAME: keeps the issue KEY's document list and its lenses as NAME, the record
# a call that writes nothing is held to (unchanged).
snapshot() {
  documents "$1" "$2" "$work/$3-documents.json"
  lenses_on "$1" "read the lenses on $2" "dispatch://$2"
  jq -cS '.' "$work/body" >"$work/$3-lenses.json"
}

# unchanged SECTION WHAT KEY NAME: the issue KEY's lenses and its document list read as the
# snapshot NAME holds them.
unchanged() {
  lenses_on "$1" "read the lenses on $3" "dispatch://$3"
  jq -cS '.' "$work/body" >"$work/lenses-now.json"
  cmp -s "$work/$4-lenses.json" "$work/lenses-now.json" ||
    fail "$1" "$2: the lenses on $3 are unchanged" "before: $(<"$work/$4-lenses.json"); after: $(<"$work/lenses-now.json")"
  pass "$1" "$2: the lenses on $3 are unchanged: $(jq -c '[.lenses[].address]' "$work/lenses-now.json")"
  documents "$1" "$3" "$work/documents-now.json"
  cmp -s "$work/$4-documents.json" "$work/documents-now.json" ||
    fail "$1" "$2: the document list of $3 is unchanged" \
      "before: $(<"$work/$4-documents.json"); after: $(<"$work/documents-now.json")"
  pass "$1" "$2: the document list of $3 is unchanged: $(jq -c '[.[].ref_key]' "$work/documents-now.json")"
}

# upload SECTION WHAT KEY NAME FILE: writes FILE as the next version of the issue KEY's document
# NAME, its answer in $work/body.
upload() {
  send POST "/api/v1/issues/$3/artifacts" "$(jq -cn --arg n "$4" --rawfile c "$5" --argjson a "$(actor)" \
    '{name: $n, content: $c, actor: $a}')"
  want_status 201 "$1" "$2"
}

# create_issue SECTION WHAT TITLE SPEC-FILE: creates an issue in $project, sets $key to it and adds
# it to the issues closed at exit.
create_issue() {
  send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$3" --rawfile s "$4" --argjson a "$(actor)" \
    '{project: $p, title: $t, spec: $s, force: true, actor: $a}')"
  want_status 201 "$1" "$2"
  key="$(jq -er '.key' "$work/body")" || fail "$1" "$2" "no key: $(body)"
  keys+=("$key")
}

# premortem_convert SECTION WHAT ARGS…: runs `envoy-dispatch premortem-convert ARGS` with
# DATABASE_URL=$STORE_DSN as a prefix of that one command; it exits 0 and prints its report, which
# goes to $work/body.
premortem_convert() {
  local section="$1" what="$2" code=0
  shift 2
  DATABASE_URL="$database_url" "$server_bin" premortem-convert "$@" >"$work/command.out" 2>"$work/command.err" || code=$?
  [[ "$code" == 0 ]] ||
    fail "$section" "$what" "envoy-dispatch premortem-convert $* exited $code: $(<"$work/command.err") $(head -c 600 "$work/command.out")"
  jq -e 'type == "object"' "$work/command.out" >/dev/null 2>&1 ||
    fail "$section" "$what" "the command printed no JSON report: $(head -c 600 "$work/command.out")"
  cp "$work/command.out" "$work/body"
  pass "$section" "$what"
}

# heading_of NUMBER: the heading the capture records for risk NUMBER.
heading_of() { jq -r --argjson n "$1" '.[] | select(.number == $n) | .heading' "$work/expected-risks.json"; }

# The verdict of a lens a conversion writes, held to the capture word for word: the capture's
# verdict, then each closing section under its own heading, as the document itself writes the two
# at its end - "## <heading>", a blank line, the section, and a blank line before the next - which
# setup reads equal to the document's own text from the first of those headings to its end.
# Nothing is normalised, because nothing stands between the two: the store keeps the verdict as one
# string attribute of the lens node, which the lens read answers as stored (api/lens_read.go reads
# attributes->>'verdict'), and the report carries the string the conversion wrote. `.` is the
# verdict; $e[0] is $work/expected-verdict.json, whose `whole` is the text it must be. A verdict
# that differs is shown from its first different character, beside the capture's text from there.
readonly verdict_filter='. as $v | $e[0].whole as $w
  | if $v == $w then "word for word"
    elif ($v | type) != "string" then {verdict: $v}
    else ([$v, $w] | map(length) | min) as $n
      | ([range(0; $n) | select($v[.:. + 1] != $w[.:. + 1])] | first // $n) as $at
      | {lengths: {stored: ($v | length), capture: ($w | length)}, first_difference: $at,
         stored_from_there: $v[$at:$at + 120], capture_from_there: $w[$at:$at + 120]}
    end'
readonly verdict_carried='"word for word"'

# would_write SECTION KEY DOCUMENT: the report in $work/body is a dry run's of the document
# DOCUMENT of the issue KEY: the lens it would write, of kind pre-mortem under the issue, with the
# verdict and both closing sections and no address; the eleven risks, each with its section's
# heading, early warning and prevention and no address; and the one prevented_by tuple of the map,
# from risk 8 to the check.
would_write() {
  expect "$1" "the dry run reports the document $3, read and not moved, and a lens of kind pre-mortem under $2 at no address" \
    "$(jq -cnS --arg d "$3" --arg on "dispatch://$2" \
      '{already_converted: false, document: $d, dry_run: true, lens: {address: null, kind: "pre-mortem", on: $on}, moved_to: null}')" \
    '{dry_run, already_converted, document: .document.address, moved_to: .document.moved_to, lens: {address: .lens.address, kind: .lens.kind, on: .lens.on}}'
  expect "$1" "the lens it would write carries the verdict and the two closing sections, word for word" "$verdict_carried" \
    --slurpfile e "$work/expected-verdict.json" ".lens.verdict | $verdict_filter"
  expect "$1" "it reports the eleven risks it would write, each with its section's heading, early warning and prevention" \
    "$(jq -cS '.' "$work/expected-risks.json")" '[.risks[] | {number, heading, early_warning, prevention}] | sort_by(.number)'
  expect "$1" "it reports the one prevented_by tuple it would write, from risk 8 to $check" \
    "$(jq -cn --arg c "$check" '[{number: 8, prevented_by: [$c]}]')" \
    '[.risks[] | select(.prevented_by != []) | {number, prevented_by}]'
  expect "$1" "it names no address for a risk it would write" '[]' '[.risks[] | select(.address != null) | .number]'
}

# --- the catalogue ------------------------------------------------------------------------------

send GET /api/v1/schema/blocks
want_status 200 "$driver" "read the catalogue"
held="$(jq -cS --argjson want "$seeded_kinds" '[.lens_kinds[].id] as $ids | [$want[] | select(. as $id | $ids | index($id))]' \
  "$work/body")" || fail "$driver" "read the catalogue" "unreadable: $(body)"
[[ "$held" == "$seeded_kinds" ]] ||
  fail "$driver" "the catalogue holds its ten kinds" "it holds $held of $seeded_kinds"
pass "$driver" "the catalogue holds its ten kinds"

# --- setup: the capture, the issue standing for INFRA-1098, its documents and the map ------------

# What the capture records of each risk's section and of the document's closing sections, read out
# of its markdown: a section runs from its heading to the next heading of its level or above, and an
# early warning or a prevention is the list item that opens with its label in bold, with the lines
# that continue it.
jq -j '.document.markdown' "$fixture" >"$work/premortem.md"
readonly document_name="graph-premortem.md"
[[ "$(jq -r '.document.name' "$fixture")" == "$document_name" ]] ||
  fail setup "the capture records the document $document_name" "it records $(jq -c '.document.name' "$fixture")"
jq -cS '
  def item($label): split("\n") as $l
    | ("^ {0,3}[-*+][ \t]+\\*\\*" + $label + "\\*\\*") as $open
    | ([range(0; $l | length) | select($l[.] | test($open))] | first) as $at
    | if $at == null then null else
        ([range($at + 1; $l | length)
          | select($l[.] | test("^[ \t]*$|^ {0,3}#{1,6}[ \t]|^ {0,3}([-*+]|[0-9]+[.)])[ \t]"))] | first // ($l | length)) as $end
        | [($l[$at] | sub($open; "")), $l[$at + 1:$end][]] | map(sub("^\\s+"; "") | sub("\\s+$"; "")) | join("\n")
      end;
  [.risks[] | {number: .index, heading: .title,
    early_warning: (.markdown | item("Early warning:")), prevention: (.markdown | item("Prevention:"))}] | sort_by(.number)' \
  "$fixture" >"$work/expected-risks.json" || fail setup "read each risk's section out of the capture" "jq refused $fixture"
# The verdict a conversion writes on its lens (verdict_filter): the capture's verdict, then each
# closing section under its own heading. Beside it, what setup holds it to: the document's own
# Verdict section, and the document's own text from the first closing heading to its end.
jq -cS '
  def section(heading): split("\n") as $l
    | ([range(0; $l | length) | select($l[.] | heading)] | first) as $at
    | if $at == null then null else
        ([range($at + 1; $l | length) | select($l[.] | test("^#{1,2}[ \t]"))] | first // ($l | length)) as $end
        | $l[$at + 1:$end] | join("\n") | sub("^([ \t]*\n)+"; "") | sub("(\n[ \t]*)+$"; "")
      end;
  def from_heading($line): split("\n") as $l
    | ([range(0; $l | length) | select($l[.] == $line)] | first) as $at
    | if $at == null then null else $l[$at:] | join("\n") | sub("(\n[ \t]*)+$"; "") end;
  "What a world-class implementation looks like" as $wc | "The smallest first slice that proves the model" as $fs
  | (.document.markdown | section(. == "## " + $wc)) as $world_class
  | (.document.markdown | section(. == "## " + $fs)) as $first_slice
  | {verdict, world_class: $world_class, first_slice: $first_slice,
     document_verdict: (.document.markdown | section(startswith("## Verdict"))),
     closing: (.document.markdown | from_heading("## " + $wc)),
     whole: (.verdict + "\n\n## " + $wc + "\n\n" + $world_class + "\n\n## " + $fs + "\n\n" + $first_slice)}' \
  "$fixture" >"$work/expected-verdict.json" || fail setup "read the verdict and the closing sections out of the capture" "jq refused $fixture"
cp "$work/expected-risks.json" "$work/body"
expect setup "the capture records risk 0 and the ten numbered risks, each with a heading, an early warning and a prevention" \
  '{"numbers":[0,1,2,3,4,5,6,7,8,9,10],"whole":true}' \
  '{numbers: [.[].number], whole: all(.[]; (.heading | type) == "string" and (.early_warning | type) == "string" and (.prevention | type) == "string")}'
cp "$work/expected-verdict.json" "$work/body"
expect setup "the capture records the verdict and the two closing sections" true \
  'all(.verdict, .world_class, .first_slice; type == "string" and length > 0)'
expect setup "the capture's verdict is the document's own Verdict section, word for word" true '.verdict == .document_verdict'
expect setup "the document ends with its two closing sections, each under its heading, word for word as the lens's verdict holds them after the verdict" \
  true '.closing != null and .whole == .verdict + "\n\n" + .closing'

cat >"$work/spec.md" <<'SPEC'
The issue standing for dispatch://INFRA-1098 in this run, whose pre-mortem is converted into nodes.

:::requirement{#r-node-visibility}
Every node kind answers through one visibility predicate.
:::
SPEC
create_issue setup "create the issue standing for INFRA-1098" "Pre-mortem conversion check: the issue standing for INFRA-1098" "$work/spec.md"
readonly key1="$key"
pass setup "created $key1 in $project to stand for dispatch://INFRA-1098"

cat >"$work/verification.md" <<VERIFICATION
How $key1 is proved.

:::check{#c-visibility-kinds serves="dispatch://$key1/spec#b-r-node-visibility"}
The census of node kinds fails a kind that answers through no visibility predicate.
:::

:::check{#c-census}
A check of $key1 that the map names for no risk.
:::
VERIFICATION
upload setup "write $key1's verification document with two checks" "$key1" verification.md "$work/verification.md"
readonly verification="dispatch://$key1/artifact/verification-md"
readonly check="$verification#b-c-visibility-kinds"
send GET "/api/v1/issues/$key1/artifacts/verification-md/blocks"
want_status 200 setup "read the blocks of $verification"
expect setup "$verification holds the checks c-visibility-kinds and c-census" \
  '[["c-census","check"],["c-visibility-kinds","check"]]' \
  '[.[] | select(.id == "c-visibility-kinds" or .id == "c-census") | [.id, .type]] | sort'

upload setup "write $key1's document $document_name holding the capture's markdown" "$key1" "$document_name" "$work/premortem.md"
slug1="$(jq -er '.artifact.slug' "$work/body")" || fail setup "write $document_name on $key1" "no slug: $(body)"
ref_key1="$(jq -er '.artifact.ref_key' "$work/body")" || fail setup "write $document_name on $key1" "no ref_key: $(body)"
readonly slug1 ref_key1 document1="dispatch://$key1/artifact/$slug1"
send GET "/api/v1/issues/$key1/artifacts/$slug1/text"
want_status 200 setup "read the text of $document1"
jq -e --rawfile md "$work/premortem.md" '.markdown == $md' "$work/body" >/dev/null ||
  fail setup "$document1 holds the capture's markdown, character for character" "it holds $(jq -c '.markdown | .[0:200]' "$work/body")…"
pass setup "wrote $document1, which holds the capture's markdown character for character"

# The map, one entry for each risk: risk 8 to the one check, and every other risk, risk 3 among
# them, to none. The file lists them from risk 10 down, since a map is read in any order, under a
# comment line and a blank line, which the command skips; the route's body lists the same entries.
jq -cn --arg c "$check" '[range(10; -1; -1) as $n
  | {risk: $n, checks: (if $n == 8 then [$c] else [] end),
     reason: (if $n == 8 then "c-visibility-kinds fails a node kind that answers through no visibility predicate."
              elif $n == 3 then "No check of this issue prevents search from regressing; nothing prevents it."
              else "No check of this issue prevents risk \($n)." end)}]' >"$work/map.json"
{
  printf '# risk\tchecks\treason\n\n'
  jq -r '.[] | "\(.risk)\t\(if .checks == [] then "none" else .checks | join(" ") end)\t\(.reason)"' "$work/map.json"
} >"$work/map.tsv"
pass setup "wrote the map file: risk 8 to $check, risk 3 and every other risk to none"

# --- C1: the command's dry run ------------------------------------------------------------------

snapshot C1 "$key1" key1
premortem_convert C1 "envoy-dispatch premortem-convert --issue $key1 --artifact $slug1 --prevented-by <map> --dry-run exits 0" \
  --issue "$key1" --artifact "$slug1" --prevented-by "$work/map.tsv" --dry-run
cp "$work/body" "$work/command-dry-run.json"
would_write C1 "$key1" "$document1"
unchanged C1 "after the dry run" "$key1" key1

# --- C2: the command's conversion: one lens and eleven risks flat under it ----------------------

premortem_convert C2 "envoy-dispatch premortem-convert --issue $key1 --artifact $slug1 --prevented-by <map> exits 0" \
  --issue "$key1" --artifact "$slug1" --prevented-by "$work/map.tsv"
cp "$work/body" "$work/command.json"
lens1="$(jq -er '.lens.address' "$work/command.json")" || fail C2 "the command reports its lens" "no lens address: $(body)"
readonly lens1
expect C2 "the command reports the lens $lens1 it wrote under $key1, and the document's address moved to it" \
  "$(jq -cnS --arg d "$document1" --arg l "$lens1" --arg on "dispatch://$key1" \
    '{already_converted: false, document: $d, dry_run: false, kind: "pre-mortem", moved_to: $l, on: $on}')" \
  '{dry_run, already_converted, document: .document.address, moved_to: .document.moved_to, kind: .lens.kind, on: .lens.on}'

lenses_on C2 "read the lenses on $key1" "dispatch://$key1"
cp "$work/body" "$work/key1-lenses-after.json"
expect C2 "one lens node hangs under $key1, the lens the command wrote" "[\"$lens1\"]" '[.lenses[].address]'
jq -c --arg l "$lens1" '.lenses[] | select(.address == $l)' "$work/key1-lenses-after.json" >"$work/lens1.json"
cp "$work/lens1.json" "$work/body"
expect C2 "$lens1 stands on $key1" "\"dispatch://$key1\"" '.on'
expect C2 "$lens1 carries the verdict and the artifact's two closing sections, word for word" "$verdict_carried" \
  --slurpfile e "$work/expected-verdict.json" ".verdict | $verdict_filter"
expect C2 "$lens1 answers the kind pre-mortem" '"pre-mortem"' '.kind.id'
references C2 "read the has_kind tuples of $lens1" from "$lens1" has_kind
expect C2 "$lens1 answers kind pre-mortem through its has_kind tuple to $lens_kind" \
  "[[\"has_kind\",\"lens_kind\",\"$lens_kind\"]]" '[.edges[] | [.kind, .node.kind, .node.ref]]'
references C2 "read the part_of tuple of $lens1" from "$lens1" part_of
expect C2 "$lens1 hangs under $key1 through its part_of tuple" "[[\"issue\",\"dispatch://$key1\"]]" \
  '[.edges[] | [.node.kind, .node.ref]]'

risk_count="$(jq '.risks | length' "$work/lens1.json")"
missing="$(jq -r --slurpfile e "$work/expected-risks.json" '[$e[0][].heading] - [.risks[].heading] | join("; ")' "$work/lens1.json")"
[[ "$risk_count" == 11 && -z "$missing" ]] ||
  fail C2 "eleven risk nodes hang under $lens1, risk 0 and the ten numbered risks" \
    "it holds $risk_count risk nodes, and no node holds the heading: ${missing:-(each heading has one)}"
pass C2 "eleven risk nodes hang under $lens1, risk 0 and the ten numbered risks"
references C2 "read the backlinks of $lens1" to "$lens1" part_of
expect C2 "the backlinks of $lens1 hold the eleven risks through their part_of tuples" \
  "$(jq -cS '[.risks[].address] | sort' "$work/lens1.json")" '[.edges[] | select(.node.kind == "risk") | .node.ref] | sort'

for number in $(jq -r '.[].number' "$work/expected-risks.json"); do
  heading="$(heading_of "$number")"
  cp "$work/lens1.json" "$work/body"
  expect C2 "risk $number's node carries its section's heading, early warning and prevention" \
    "$(jq -cS --argjson n "$number" '[.[] | select(.number == $n) | {heading, early_warning, prevention}]' "$work/expected-risks.json")" \
    --arg h "$heading" '[.risks[] | select(.heading == $h) | {heading, early_warning, prevention}]'
  address="$(jq -r --arg h "$heading" '.risks[] | select(.heading == $h) | .address' "$work/lens1.json")"
  printf '%s' "$address" >"$work/risk1-$number"
  references C2 "read the tuples out of risk $number, $address" from "$address"
  expect C2 "risk $number hangs flat under $lens1: its parent is the lens, and its one part_of tuple points there" \
    "$(jq -cn --arg l "$lens1" '{parent: $l, part_of: [$l]}')" \
    '{parent: .node.parent.ref, part_of: [.edges[] | select(.kind == "part_of") | .node.ref]}'
done

# --- C3: the risks' addresses, risk 8's check and risk 3's nothing -------------------------------

cp "$work/lens1.json" "$work/body"
expect C3 "each risk is at an address of its own, dispatch://risk/<uuid>" true \
  '[.risks[].address] as $a | ($a | unique | length) == 11
    and all($a[]; test("^dispatch://risk/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))'
for number in $(jq -r '.[].number' "$work/expected-risks.json"); do
  address="$(<"$work/risk1-$number")"
  references C3 "read risk $number at $address" to "$address"
  expect C3 "risk $number answers at $address, which the store gave it" \
    "$(jq -cn --arg a "$address" '{kind: "risk", ref: $a}')" '.node | {kind, ref}'
  numbered="dispatch://$key1/premortem#risk-$number"
  send GET "/api/v1/references?to=$(q "$numbered")"
  want_status 400 C3 "Dispatch refuses $numbered as a reference"
  expect C3 "risk $number answers at no address $numbered, which Dispatch refuses as a reference" \
    '"INVALID_REFERENCE"' '.code'
done

risk8="$(<"$work/risk1-8")"
risk3="$(<"$work/risk1-3")"
readonly risk8 risk3
references C3 "read the prevented_by tuples of risk 8, $risk8" from "$risk8" prevented_by
expect C3 "the prevented_by tuple of risk 8 reaches the check the map file names, $check" "[\"$check\"]" \
  '[.edges[] | .node.ref]'
references C3 "read $check" to "$check"
expect C3 "$check exists: a live block of $verification" \
  "$(jq -cnS --arg d "$verification" '{document: $d, kind: "block", state: "live"}')" '.node | {document, kind, state}'
expect C3 "the backlinks of $check hold risk 8 through its prevented_by tuple" "[[\"prevented_by\",\"$risk8\"]]" \
  '[.edges[] | select(.node.kind == "risk") | [.kind, .node.ref]]'
# The absence beside its control: the same read of risk 8 found its tuple.
references C3 "read the prevented_by tuples of risk 3, $risk3" from "$risk3" prevented_by
expect C3 "risk 3 reads as prevented by nothing" '[]' '[.edges[] | .node.ref]'
cp "$work/lens1.json" "$work/body"
expect C3 "the lens read answers risk 8 prevented by $check and every other risk, risk 3 among them, by nothing" \
  "$(jq -cn --arg r "$risk8" --arg c "$check" '[[$r, [$c]]]')" \
  '[.risks[] | select(.prevented_by != []) | [.address, .prevented_by]]'
expect C3 "the lens read answers risk 3 prevented by nothing" '[[]]' --arg r "$risk3" \
  '[.risks[] | select(.address == $r) | .prevented_by]'

# --- R1: the route refuses an entry for a risk the document does not hold -----------------------

cat >"$work/spec2.md" <<'SPEC'
The second issue of this run, whose pre-mortem the route converts.
SPEC
create_issue R1 "create the second issue" "Pre-mortem conversion check: the issue the route converts" "$work/spec2.md"
readonly key2="$key"
upload R1 "write $key2's document $document_name holding the same markdown" "$key2" "$document_name" "$work/premortem.md"
slug2="$(jq -er '.artifact.slug' "$work/body")" || fail R1 "write $document_name on $key2" "no slug: $(body)"
ref_key2="$(jq -er '.artifact.ref_key' "$work/body")" || fail R1 "write $document_name on $key2" "no ref_key: $(body)"
readonly slug2 ref_key2 document2="dispatch://$key2/artifact/$slug2"
readonly route="/api/v1/issues/$key2/artifacts/$slug2/premortem-convert"
pass R1 "created $key2 with the pre-mortem document $document2"

snapshot R1 "$key2" key2
send POST "$route" "$(jq -cn --slurpfile m "$work/map.json" --argjson a "$(actor)" \
  '{prevented_by: ($m[0] + [{risk: 11, checks: [], reason: "A risk the document does not hold."}]), actor: $a}')"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
unchanged R1 "after the call with an entry for risk 11" "$key2" key2
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 R1 "the call with the eleven entries and one for risk 11 is refused"
expect R1 "the refusal is ARTIFACT_INPUT" '"ARTIFACT_INPUT"' '.code'
expect R1 "the refusal names the entry for risk 11, the twelfth, which the document does not hold" true \
  '.error | contains("prevented_by entry 12 (risk 11)") and contains("the document holds no risk 11")'

# --- R2: the route's dry run --------------------------------------------------------------------

send POST "$route" "$(jq -cn --slurpfile m "$work/map.json" --argjson a "$(actor)" '{prevented_by: $m[0], dry_run: true, actor: $a}')"
cp "$work/body" "$work/route-dry-run.json"
previewed_status="$status"
lenses_on R2 "read the lenses on $key2 after the call with dry_run" "dispatch://$key2"
written="$(jq -r '[.lenses[].address] | join(", ")' "$work/body")"
[[ -z "$written" ]] ||
  fail R2 "the call with dry_run writes no lens under $key2" \
    "the lens $written stands under $key2 after it; the call answered HTTP $previewed_status: $(head -c 600 "$work/route-dry-run.json")"
pass R2 "the call with dry_run wrote no lens under $key2"
cp "$work/route-dry-run.json" "$work/body"
status="$previewed_status"
want_status 200 R2 "the call with the eleven entries and dry_run answers"
would_write R2 "$key2" "$document2"
unchanged R2 "after the call with dry_run" "$key2" key2
references R2 "read $document2, its old address" to "$document2"
expect R2 "$document2 still answers the document after the dry run" \
  "$(jq -cn --arg d "$document2" '{kind: "artifact", ref: $d}')" '.node | {kind, ref}'

# --- R3: the route's conversion, compared with the command's by content -------------------------

send POST "$route" "$(jq -cn --slurpfile m "$work/map.json" --argjson a "$(actor)" '{prevented_by: $m[0], actor: $a}')"
want_status 200 R3 "the call with the eleven entries and no dry_run converts $document2"
cp "$work/body" "$work/route.json"
lens2="$(jq -er '.lens.address' "$work/route.json")" || fail R3 "the route reports its lens" "no lens address: $(body)"
readonly lens2
expect R3 "the route reports the lens $lens2 it wrote under $key2, and the document's address moved to it" \
  "$(jq -cnS --arg d "$document2" --arg l "$lens2" --arg on "dispatch://$key2" \
    '{already_converted: false, document: $d, dry_run: false, kind: "pre-mortem", moved_to: $l, on: $on}')" \
  '{dry_run, already_converted, document: .document.address, moved_to: .document.moved_to, kind: .lens.kind, on: .lens.on}'
# By content alone: the command writes as the server and the route as its caller, so the
# addresses, made_by, the author and the time differ.
readonly report_content='{lens: {kind: .lens.kind, verdict: .lens.verdict},
  risks: [.risks[] | {number, heading, early_warning, prevention, prevented_by, reason}]}'
expect R3 "the route reports the lens and the eleven risks the command reported, each compared by content" \
  "$(jq -cS "$report_content" "$work/command.json")" "$report_content"

lenses_on R3 "read the lenses on $key2" "dispatch://$key2"
expect R3 "one lens node hangs under $key2, the lens the route wrote" "[\"$lens2\"]" '[.lenses[].address]'
jq -c --arg l "$lens2" '.lenses[] | select(.address == $l)' "$work/body" >"$work/lens2.json"
readonly lens_content='{verdict, risks: [.risks[] | {heading, early_warning, prevention, prevented_by}] | sort_by(.heading)}'
cp "$work/lens2.json" "$work/body"
expect R3 "$lens2 carries the verdict of $lens1, and the same eleven risks with the same prevented_by far ends" \
  "$(jq -cS "$lens_content" "$work/lens1.json")" "$lens_content"
references R3 "read the has_kind tuples of $lens2" from "$lens2" has_kind
expect R3 "$lens2 answers kind pre-mortem through its has_kind tuple to $lens_kind, as $lens1 does" \
  "[[\"has_kind\",\"lens_kind\",\"$lens_kind\"]]" '[.edges[] | [.kind, .node.kind, .node.ref]]'
cp "$work/lens2.json" "$work/body"
expect R3 "the route's nodes are new: $lens2 is not $lens1, and no risk of either is the other's" true \
  --slurpfile one "$work/lens1.json" \
  '.address != $one[0].address and ([.risks[].address] - [$one[0].risks[].address] | length) == 11'

# --- M: the moved_to tuples, read through psql on STORE_DSN -------------------------------------

for converted in 1 2; do
  case "$converted" in
    1) ref_key="$ref_key1" document="$document1" lens="$lens1" ;;
    *) ref_key="$ref_key2" document="$document2" lens="$lens2" ;;
  esac
  [[ "$ref_key" =~ ^[A-Za-z0-9-]+/[a-z0-9-]+$ ]] || fail M "read the moved_to tuples of $document" "its ref_key is $ref_key"
  query M "read the moved_to tuples from $document" \
    "select coalesce(string_agg(to_kind || '/' || to_id || ' asserted=' || asserted, ', ' order by seq), '')
       from graph_tuples where from_kind = 'artifact' and from_id = '$ref_key' and kind = 'moved_to'"
  [[ "$answer" == "lens/${lens#dispatch://lens/} asserted=true" ]] ||
    fail M "one moved_to tuple runs from $document to its own lens $lens" "the store holds: ${answer:-no moved_to tuple}"
  pass M "one moved_to tuple runs from $document's address to its own lens $lens"
done

# --- D: each document retired, its old address a redirect to its lens ---------------------------

for converted in 1 2; do
  case "$converted" in
    1) key="$key1" document="$document1" ref_key="$ref_key1" lens="$lens1" ;;
    *) key="$key2" document="$document2" ref_key="$ref_key2" lens="$lens2" ;;
  esac
  documents D "$key" "$work/documents-retired.json"
  cp "$work/documents-retired.json" "$work/body"
  # The absence beside its control: the same list holds the issue's spec.
  expect D "the document list of $key holds its spec, and not $document, which is retired" \
    "$(jq -cn --arg k "$key" '{retired: [], spec: ["\($k)/spec"]}')" --arg k "$key" --arg r "$ref_key" \
    '{spec: [.[] | select(.ref_key == "\($k)/spec") | .ref_key], retired: [.[] | select(.ref_key == $r) | .ref_key]}'
  # The redirect is the shape a moved block's old address answers (dispatch://INFRA-1150's
  # movedBlockNode): the node the move reaches, named by its own address, as `.node`. A retired
  # document's address with no move answers its issue instead (refs' loadArtifactNodes), so the
  # lens here is the conversion's moved_to and nothing else.
  references D "read $document, its old address" to "$document"
  expect D "$document answers a redirect to the lens $lens, not the issue a retired document's address answers" \
    "$(jq -cn --arg l "$lens" '{kind: "lens", ref: $l}')" '.node | {kind, ref}'
  lenses_on D "follow the redirect of $document to the lens" "$(jq -r '.node.ref' "$work/body")"
  expect D "the redirect of $document reaches $lens, whose eleven risks a reader walks down to" \
    "$(jq -cn --arg l "$lens" '{address: $l, risks: 11}')" '{address: .lens.address, risks: (.lens.risks | length)}'
  expect D "walking down from $lens reaches risk 8 by its heading" 1 \
    --arg h "$(heading_of 8)" '[(.lens.risks // [])[] | select(.heading == $h)] | length'
done

pass "$driver" complete
