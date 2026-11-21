# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # restricted-sweep.sh sources this file: it sets what this reads, and reads what this sets
# The restricted sweep's helpers (restricted-sweep.sh sources this file, beside it): what every
# part of the run reads and writes through - its requests, the lines it prints and records, and
# the tests of a status. It defines functions alone; they read the entry script's `work`,
# `origin`, `pace`, `call_seconds` and `project`.

is_count() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }
is_seconds() { case $1 in '' | *[!0-9.]* | .* | *. | *.*.*) return 1 ;; esac; }
holds_line_break() { case $1 in *$'\n'* | *$'\r'*) return 0 ;; esac; return 1; }

is_2xx() { case $1 in 2??) return 0 ;; esac; return 1; }
is_refusal() { case $1 in 403 | 404) return 0 ;; esac; return 1; }
# is_judgeable STATUS: a status some rule reads, which 401, 429, a 5xx and no answer are not.
is_judgeable() { case $1 in 401 | 429 | 5?? | curl*) return 1 ;; [1-4]??) return 0 ;; esac; return 1; }

calls=0
# call WHO PATH OUT [JSON FILE]: GET PATH as WHO (full, restricted, or full-browser, the full
# member's cookie), or with JSON FILE a POST of that file, the body in OUT and the status in
# $status, or `curl failed: ...` when no answer came. A cookie goes with the origin's Origin, as the
# dashboard sends it; curl reads no ~/.curlrc (-q), no proxy, and no URL pattern (-g).
call() {
  local -a credential data
  data=()
  case $1 in
    full) credential=(-H "@$work/full.h") ;;
    *) credential=(-H "@$work/$1.h" -H "Origin: $origin") ;;
  esac
  [ -z "${4:-}" ] || data=(-X POST -H 'Content-Type: application/json' --data-binary "@$4")
  [ "$pace" = 0 ] || sleep "$pace" || unknown 'SWEEP_PACE_SECONDS is not a duration sleep accepts'
  : >"$3" || unknown 'could not create the response body file'
  calls=$((calls + 1))
  status=$(curl -q -sS -g --noproxy '*' --max-time "$call_seconds" -o "$3" -w '%{http_code}' \
    "${credential[@]}" ${data[@]+"${data[@]}"} "$origin$2" </dev/null 2>"$work/curl.err") ||
    status="curl failed: $(tr '\n' ' ' <"$work/curl.err" | cut -c1-200)"
}

uri() { jq -rn --arg v "$1" '$v|@uri'; }
# first_of FILE FILTER prints the first value jq's FILTER yields of FILE, or nothing.
first_of() { jq -r "first($2) // empty" "$1" 2>/dev/null; }
# post_as WHO PATH REQUEST: POSTs REQUEST (a file) as WHO, and sets posted to the id the server
# answered 201 with, or nothing, the answer in the run's `body` and its status in `status`.
post_as() {
  posted=''
  call "$1" "$2" "$work/body" "$3"
  [ "$status" != 201 ] || posted=$(first_of "$work/body" '.id | select(type == "string" and length > 0)')
}
# word_before FILE REST: the first word of FILE's first line whose text after that word and one
# space is REST, or nothing: a row's status by its label, a found value's kind by the value.
word_before() {
  awk -v l="$2" 'index($0, " ") && substr($0, index($0, " ") + 1) == l { print substr($0, 1, index($0, " ") - 1); exit }' \
    "$1"
}
# Redact credentials before shortening diagnostics, including individually reflected cookie values.
redacted() {
  local text=$1 header secret rest part
  for header in full restricted full-browser; do
    [ -r "$work/$header.h" ] || continue
    IFS= read -r secret <"$work/$header.h" || continue
    secret=${secret#*: }
    text=${text//"$secret"/<redacted>}
    if [ "$header" = full ]; then
      secret=${secret#Bearer }
      [ -z "$secret" ] || text=${text//"$secret"/<redacted>}
    else
      rest=$secret
      while [ -n "$rest" ]; do
        part=${rest%%;*}
        secret=${part#*=}
        [ -z "$secret" ] || text=${text//"$secret"/<redacted>}
        [ "$part" != "$rest" ] || break
        rest=${rest#*;}
      done
    fi
  done
  printf '%s' "$text"
}
head_of() { redacted "$(cat "$1" 2>/dev/null)" | tr '\n' ' ' | cut -c1-"${2:-200}"; }
fail() {
  local message
  message=$(redacted "$1")
  printf 'FAIL    %s\n' "$message"
  printf '%s\n' "$message" >>"$work/failed" || unknown 'could not record a failed check'
}
unproven() {
  local message
  message=$(redacted "$1")
  printf 'UNKNOWN %s\n' "$message"
  printf '%s\n' "$message" >>"$work/unknown" || unknown 'could not record an unproven check'
}
# missing_kind LABEL WHY: a kind of object the project owns that the lookup did not find, which
# leaves the run UNKNOWN.
missing_kind() {
  printf 'missing %s: %s\n' "$1" "$2"
  printf '%s: %s\n' "$1" "$2" >>"$work/missing" || unknown 'could not record a missing kind'
}

# frames_of FILE: the type of each frame a socket probe printed, and its id where it has one, or
# the first two words of a line that is no frame (`closed 1011`, `http 404`). A line about the
# full member's feed quotes this and never the frames, which hold what the project holds.
frames_of() {
  jq -R -r -s 'split("\n") | map(select(length > 0)
      | (fromjson? // null) as $frame
      | if ($frame | type) == "object"
        then ($frame.type // "untyped" | tostring)
          + (if $frame.id == null then "" else " " + ($frame.id | tostring) end)
        else split(" ") | .[0:2] | join(" ") end)
    | if length == 0 then "nothing" else join(", ") end' "$1" 2>/dev/null || printf 'nothing it could read\n'
}

# alive PID: whether a socket probe of this run is still running; no probe is none.
alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }

# The sweep's writes, each to the file it names: a comment on SWEEP_WRITE_ISSUE, and a direct
# message that names no issue, both as the server reads them (`delivery` is how the listener hands
# a message to its session: `btw`, beside the session's own turn). Each says what it is and when
# it was written.
comment_request() {
  jq -cn --arg b "restricted-sweep: a probe of who is sent this comment live, written $(date -u +%Y-%m-%dT%H:%M:%SZ)." \
    '{body: $b, actor: {kind: "session", id: "restricted-sweep"}}' >"$1" ||
    unknown 'could not write the live comment request'
}
message_request() {
  jq -cn --arg b "restricted-sweep: a probe of who is sent this direct message live, written $(date -u +%Y-%m-%dT%H:%M:%SZ)." \
    '{body: $b, delivery: "btw"}' >"$1" || unknown 'could not write the live direct message request'
}

utc_of() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }

# sha256_of: the SHA-256 of its input in hex, or nothing where neither sha256sum nor shasum runs.
sha256_of() {
  local sum
  if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum) || return 1
  elif command -v shasum >/dev/null 2>&1; then
    sum=$(shasum -a 256) || return 1
  else
    return 1
  fi
  printf '%s' "${sum%% *}"
}

# How long after a write the writer waits, at most, for the full member's feed to be sent it, and
# so the margin a runner's live window leaves before its feed closes: a write the writer saw
# delivered strictly inside the window reached the feeds while the member's was open.
readonly delivery_seconds=15
