#!/usr/bin/env bash
# Captures, from a Dispatch deployment's event log (GET /api/v1/events/log), what the inventory
# tests of package textversions read, into this directory:
#
#   capture.sh types
#     pages the whole log from its first event, 1,000 events a request, one request at a time,
#     and writes production_event_types.txt: one line per event type the log holds,
#     `<type><TAB><count>`, sorted by type, under a comment line naming the date and the last
#     event id read.
#
#   capture.sh shapes INVENTORY
#     reads INVENTORY, the TSV of payload shapes that ../../testdata/event-log-capture.sh
#     describes (shape_rows). Of the rows of the types the keyword fill reads (fill_types below),
#     it fetches each shape's first event and writes it, redacted, as payloads/<type>-<n>.json, n
#     counting from 1 per type in first_id order, and beside it payloads/<type>-<n>.expected.json,
#     the version that event starts as the first event of its item, which jq reads from the
#     redacted event alone (expected below). The directory payloads/ is replaced whole.
#
# The request, its token and the redaction are ../../testdata/event-log-capture.sh's, which
# asof/testdata/capture.sh uses too; its header says what they keep and what they replace.
set -euo pipefail

# The event types that can start a version of an item's searchable text (rules version 2).
fill_types="issue.created issue.updated issue.closed artifact.version comment.created comment.answered comment.edited comment.deleted message.created message.answered message.edited message.deleted ask.opened ask.edited ask.answered"

# expected reads the version a redacted event starts as the first event of its item: its kind
# and id, its issue, its text (an issue's title, a post's body, an ask's question), an ask's
# options and answer, the issue's primary document and its version, and whether the event cut the
# body, each as the event carries it and null where it carries none. An issue event's empty
# primary_artifact_id, which the API writes for an issue with no primary document, names none, as
# an absent one does. The delete of a post starts a version with empty text, which Go reads as
# null, as it reads every empty text (expectedOf); its event carries no body. An issue.created's
# version holds version 1 of the issue's primary document, which the create writes and which gets
# no event of its own; an artifact.version's is the version it names, of the issue its envelope
# names. Read in Go, a body_truncated of null is false. It is null for an event that starts no
# version: an artifact.version of no issue (a project's document, never an issue's primary one)
# and a message of no issue, which the keyword table leaves out.
read -r -d '' expected <<'JQ' || true
.type as $type | .payload as $p
| if $type == "issue.created" or $type == "issue.updated" or $type == "issue.closed" then
    ($p.primary_artifact_id | if . == "" then null else . end) as $primary
    | {kind: "issue", id: $p.key, issue_key: $p.key, title_or_body: $p.title, options: null,
       answer: null, spec_artifact_id: $primary,
       spec_version: (if $type == "issue.created" and $primary != null then 1 else null end),
       body_truncated: null}
  elif ($type == "artifact.version" or ($type | startswith("message."))) and .issue_key == null then
    null
  elif $type == "artifact.version" then
    {kind: "issue", id: .issue_key, issue_key: .issue_key, title_or_body: null, options: null,
     answer: null, spec_artifact_id: $p.artifact_id, spec_version: $p.version.number,
     body_truncated: null}
  elif $type == "comment.deleted" or $type == "message.deleted" then
    {kind: ($type | split(".")[0]), id: $p.id, issue_key: $p.issue_key, title_or_body: null,
     options: null, answer: null, spec_artifact_id: null, spec_version: null,
     body_truncated: null}
  elif ($type | startswith("comment.")) or ($type | startswith("message.")) then
    {kind: ($type | split(".")[0]), id: $p.id, issue_key: $p.issue_key, title_or_body: $p.body,
     options: null, answer: null, spec_artifact_id: null, spec_version: null,
     body_truncated: $p.body_truncated}
  elif ($type | startswith("ask.")) then
    {kind: "ask", id: $p.id, issue_key: $p.issue_key, title_or_body: $p.question,
     options: $p.options, answer: $p.answer, spec_artifact_id: null, spec_version: null,
     body_truncated: null}
  else error("no version is expected of an event of type \($type)") end
JQ

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../../testdata/event-log-capture.sh
source -- "$here/../../testdata/event-log-capture.sh"

capture_types() {
  local after=0 seen=0 rows=$work/rows types=$work/types out=$work/types.txt n
  : >"$types"
  while :; do
    fetch "$after" 1000 "$work/page.json"
    jq -r '.[] | [.id, .type] | @tsv' "$work/page.json" >"$rows"
    n=$(wc -l <"$rows")
    ((n > 0)) || break
    cut -f2 "$rows" >>"$types"
    after=$(tail -n 1 "$rows" | cut -f1)
    seen=$((seen + n))
    if ((seen % 50000 < n)); then
      echo "capture.sh: $seen events read, through event $after" >&2
    fi
  done
  {
    printf '# read %s through event id %s: %s events of %s types, <type><TAB><count> per line\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$after" "$seen" "$(LC_ALL=C sort -u "$types" | wc -l)"
    LC_ALL=C sort "$types" | uniq -c | awk '{ printf "%s\t%s\n", $2, $1 }'
  } >"$out"
  mv -- "$out" "$here/production_event_types.txt"
  echo "capture.sh: wrote production_event_types.txt, $seen events through event $after" >&2
}

capture_shapes() {
  local inventory=$1 out=$work/payloads shapes=$work/shapes.tsv type first_id keys n=0 previous="" name
  shape_rows "$inventory" "$fill_types" >"$shapes"
  [[ -s $shapes ]] || {
    echo "capture.sh: the inventory names no shape of the types the fill reads" >&2
    exit 1
  }
  mkdir -- "$out"
  while IFS=$'\t' read -r type first_id keys <&3; do
    if [[ $type != "$previous" ]]; then
      n=0
      previous=$type
    fi
    n=$((n + 1))
    name=$type-$n
    capture_first_event "$type" "$first_id" "$keys" "$name" "$out/$name.json"
    jq "$expected" "$out/$name.json" >"$out/$name.expected.json"
  done 3<"$shapes"
  rm -rf -- "$here/payloads"
  mv -- "$out" "$here/payloads"
  echo "capture.sh: wrote $(wc -l <"$shapes") shapes of $(cut -f1 "$shapes" | sort -u | wc -l) types into payloads/" >&2
}

main() {
  case ${1:-} in
  types) capture_types ;;
  shapes) capture_shapes "${2:?usage: capture.sh shapes INVENTORY}" ;;
  *)
    echo "usage: capture.sh types | capture.sh shapes INVENTORY" >&2
    exit 2
    ;;
  esac
}

# One line, so bash has read all of it before main runs: an edit of this file while a capture
# runs cannot change what the run does next.
main "$@"; exit
