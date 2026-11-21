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
#     reads INVENTORY, a TSV of payload shapes with the columns type, first_id, first_at, events
#     and payload_keys, one row per event type and top-level key set of its payload, first_id the
#     id of the first event of that shape. Of the rows of the types the keyword fill reads
#     (fill_types below), it fetches each shape's first event and writes it, redacted, as
#     payloads/<type>-<n>.json, n counting from 1 per type in first_id order, and beside it
#     payloads/<type>-<n>.expected.json, the version that event starts as the first event of its
#     item, which jq reads from the redacted event alone (expected below). A line whose first_id
#     is not a number, such as the header, is skipped. The directory payloads/ is replaced whole.
#
# DISPATCH_URL names the deployment and DISPATCH_TOKEN_FILE a file holding a bearer token it
# accepts. The token reaches curl on its standard input (-H @-), never in its arguments, which
# another process can read. A request that fails, or answers anything but a JSON array, is sent
# again, up to five times.
#
# The redaction (redact below) reads the whole event as the API answers it, its envelope and its
# payload, and keeps its key sets, nesting, numbers, booleans and nulls. Of its strings:
#   - kept as they are: uuids, RFC 3339 times, numbers written as strings, the empty string, the
#     values of the keys that name a closed set of words (type, kind, state, status, role,
#     ask_state, ask_waiting_on, turn, urgency, mode, delivery, changed and
#     references_changed_truncated_kinds), and an id or block_id that is a block's number (b2);
#   - the value of a project or project_key becomes ACME;
#   - an issue key becomes ACME-<n>, n counting from 1 per event in the order the keys first
#     appear in it, so one issue keeps one key throughout an event and two keep two;
#   - a dispatch:// link becomes dispatch://ACME-<n>, the key of the issue it names, or
#     dispatch://ACME when it names none;
#   - an email becomes alice@example.com, bob@example.com and on, one name per person in the
#     order the people first appear in the event, read by the part before the @, so a bare login
#     of the same person (a value of assignee, owner, user, login, thread_logins, editors,
#     asked_of, asked_of_by or resolved_by that is no email) becomes the same name alone;
#   - a session:, role: or user: target keeps its prefix, and its rest is redacted as a string;
#   - every other string becomes neutral text of the same length in UTF-16 units, so a body the
#     event cut stays cut at its length.
# So no text, title, name, path, host, repository or session title of the deployment survives.
set -euo pipefail

# The event types that can start a version of an item's searchable text (rules version 1).
fill_types="issue.created issue.updated issue.closed artifact.version comment.created comment.answered comment.edited message.created message.answered ask.opened ask.edited ask.answered"

read -r -d '' redact <<'JQ' || true
def uuid: test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$");
def rfc3339: test("^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})$");
def numeral: test("^-?[0-9]+(\\.[0-9]+)?$");
def issue_key: test("^[A-Z][A-Z0-9]*-[0-9]+$");
def link: test("^dispatch://"; "i");
def link_key: [capture("^dispatch://(?<key>[A-Za-z][A-Za-z0-9]*-[0-9]+)([^A-Za-z0-9]|$)"; "i") | .key | ascii_upcase] | first;
def email: test("^[^@\\s]+@[^@\\s]+$");
def login_part: split("@")[0] | ascii_downcase;
def units: [explode[] | if . > 65535 then 2 else 1 end] | add // 0;
def neutral($n): if $n == 0 then "" else ("lorem ipsum dolor sit amet " * (($n / 27 | floor) + 1))[0:$n] end;
def first_seen: reduce .[] as $x ([]; if any(.[]; . == $x) then . else . + [$x] end);
def numbered(f): first_seen | to_entries | map({key: .value, value: (.key | f)}) | from_entries;
def names: ["alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy",
  "mallory", "niaj", "olivia", "peggy", "rupert", "sybil", "trent", "victor", "walter"];
def word_keys: ["type", "kind", "state", "status", "role", "ask_state", "ask_waiting_on", "turn",
  "urgency", "mode", "delivery", "changed", "references_changed_truncated_kinds"];
def login_keys: ["assignee", "owner", "user", "login", "thread_logins", "editors", "asked_of",
  "asked_of_by", "resolved_by"];
def project_keys: ["project", "project_key"];

[paths(type == "string") as $p | {key: ($p | map(select(type == "string")) | last), value: getpath($p)}] as $leaves
| ($leaves | map(.value | if issue_key then . elif link then (link_key // empty) else empty end)
    | numbered("ACME-\(. + 1)")) as $issues
| ($leaves | map(if (.value | email) then (.value | login_part)
      elif (.key | IN(login_keys[])) and .value != "" and (.value | uuid | not) then (.value | login_part)
      else empty end)
    | numbered(. as $i | names[$i] // "user\($i + 1)")) as $logins
| def text($key):
    if . == "" or uuid or rfc3339 or numeral then .
    elif ($key | IN(project_keys[])) then "ACME"
    elif ($key | IN(word_keys[])) then .
    elif ($key == "id" or $key == "block_id") and test("^b[0-9]+$") then .
    elif issue_key then $issues[.]
    elif link then "dispatch://" + (link_key as $k | if $k == null then "ACME" else $issues[$k] end)
    elif email then $logins[login_part] + "@example.com"
    elif ($key | IN(login_keys[])) then $logins[login_part]
    elif test("^(session|role|user):.") then (capture("^(?<prefix>[a-z]+:)(?<rest>.*)$"; "s") | .prefix + (.rest | text("")))
    else neutral(units) end;
  reduce (paths(type == "string")) as $p (.; setpath($p; getpath($p) | text($p | map(select(type == "string")) | last)))
JQ

# expected reads the version a redacted event starts as the first event of its item: its kind
# and id, its issue, its text (an issue's title, a post's body, an ask's question), an ask's
# options and answer, the issue's primary document and its version, and whether the event cut
# the body, each as the event carries it and null where it carries none. An issue event's empty
# primary_artifact_id, which the API writes for an issue with no primary document, names none, as
# an absent one does. An issue.created's version holds version 1 of the issue's primary document,
# which the create writes and which gets no event of its own; an artifact.version's is the
# version it names, of the issue its envelope names. Read in Go, a body_truncated of null is
# false. It is null for an event that starts no version: an artifact.version of no issue (a
# project's document, never an issue's primary one) and a message of no issue, which the keyword
# table leaves out.
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
url=${DISPATCH_URL:?DISPATCH_URL names the Dispatch deployment to read}
url=${url%/}
token_file=${DISPATCH_TOKEN_FILE:?DISPATCH_TOKEN_FILE names the file holding the bearer token}
[[ -r $token_file ]] || {
  echo "capture.sh: cannot read DISPATCH_TOKEN_FILE" >&2
  exit 2
}

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

# fetch AFTER LIMIT OUT writes to OUT the page of the log after event id AFTER, at most LIMIT
# events, as the API answers it.
fetch() {
  local after=$1 limit=$2 out=$3 attempt
  for attempt in 1 2 3 4 5; do
    if printf 'Authorization: Bearer %s\n' "$(<"$token_file")" |
      curl -sS --fail --max-time 120 -H @- -o "$out" \
        "$url/api/v1/events/log?after=$after&limit=$limit" &&
      jq -e 'type == "array"' "$out" >/dev/null 2>&1; then
      return 0
    fi
    echo "capture.sh: the page after event $after failed (attempt $attempt of 5)" >&2
    sleep $((attempt * 2))
  done
  echo "capture.sh: the page after event $after failed five times" >&2
  return 1
}

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
  [[ -r $inventory ]] || {
    echo "capture.sh: cannot read the inventory $inventory" >&2
    exit 2
  }
  awk -F '\t' -v types="$fill_types" '
    BEGIN { split(types, listed, " "); for (i in listed) fill[listed[i]] = 1 }
    ($1 in fill) && $2 ~ /^[0-9]+$/ { print $1 "\t" $2 "\t" $5 }
  ' "$inventory" | LC_ALL=C sort -t $'\t' -k1,1 -k2,2n >"$shapes"
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
    fetch "$((first_id - 1))" 1 "$work/event.json"
    jq -e --arg type "$type" --argjson id "$first_id" \
      'length == 1 and .[0].id == $id and .[0].type == $type' "$work/event.json" >/dev/null || {
      echo "capture.sh: the log holds no $type event $first_id" >&2
      exit 1
    }
    jq --arg keys "$keys" -r '.[0].payload | keys | join(",") | select(. != $keys)' "$work/event.json" |
      while read -r answered; do
        echo "capture.sh: $name: the inventory names the keys $keys, the event carries $answered" >&2
      done
    jq ".[0] | $redact" "$work/event.json" >"$out/$name.json"
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
