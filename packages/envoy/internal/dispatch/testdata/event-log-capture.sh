# shellcheck shell=bash disable=SC2034 # the capture scripts read url, token_file, work and redact
# packages/envoy/internal/dispatch/testdata/event-log-capture.sh
#
# The part of a capture from a Dispatch deployment's event log (GET /api/v1/events/log) that every
# package's fixtures share: the request, the shape list and the redaction. It is sourced, never
# run, by textversions/testdata/capture.sh and asof/testdata/capture.sh, which name the event
# types each package reads and where its fixtures go. Sourced, it checks the two variables below,
# makes a work directory that the script's exit deletes, and defines:
#
#   fetch AFTER LIMIT OUT
#     writes to OUT the page of the log after event id AFTER, at most LIMIT events, as the API
#     answers it.
#
#   shape_rows INVENTORY TYPES
#     prints, of INVENTORY, the rows of the types that the space-separated list TYPES names, as
#     `<type><TAB><first_id><TAB><payload_keys>`, sorted by type and then by first_id. INVENTORY is
#     a TSV of payload shapes with the columns type, first_id, first_at, events and payload_keys,
#     one row per event type and top-level key set of its payload, first_id the id of the first
#     event of that shape. A line whose first_id is not a number, such as the header, is skipped.
#
#   capture_first_event TYPE FIRST_ID KEYS NAME OUT
#     fetches the event FIRST_ID, fails unless the log holds it with type TYPE, warns (naming NAME)
#     when its payload's key set is not KEYS, and writes the event to OUT, redacted.
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

url=${DISPATCH_URL:?DISPATCH_URL names the Dispatch deployment to read}
url=${url%/}
token_file=${DISPATCH_TOKEN_FILE:?DISPATCH_TOKEN_FILE names the file holding the bearer token}
[[ -r $token_file ]] || {
  echo "capture.sh: cannot read DISPATCH_TOKEN_FILE" >&2
  exit 2
}

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

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

shape_rows() {
  local inventory=$1 types=$2
  [[ -r $inventory ]] || {
    echo "capture.sh: cannot read the inventory $inventory" >&2
    exit 2
  }
  awk -F '\t' -v types="$types" '
    BEGIN { split(types, listed, " "); for (i in listed) read[listed[i]] = 1 }
    ($1 in read) && $2 ~ /^[0-9]+$/ { print $1 "\t" $2 "\t" $5 }
  ' "$inventory" | LC_ALL=C sort -t $'\t' -k1,1 -k2,2n
}

capture_first_event() {
  local type=$1 first_id=$2 keys=$3 name=$4 out=$5 answered
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
  jq ".[0] | $redact" "$work/event.json" >"$out"
}
