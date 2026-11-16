#!/usr/bin/env bash
# The driver of the check c-outside-path-reach (dispatch://INFRA-3862): a person from outside runs
# it on their own device, over the path we give them, to prove that path reaches Dispatch and
# nothing else of ours.
#
# It is handed to that person as this one file with its target list, so it sources nothing and
# needs only bash (3.2, as macOS ships it, or later) and curl 7.40 or later: no GNU `timeout`,
# which stock macOS lacks, since curl's own --connect-timeout and --max-time bound every probe. No
# curl it runs reads the terminal (stdin is /dev/null), the device's curl configuration (`-q`
# ignores ~/.curlrc, whose `header`, `proxy`, `resolve` or `connect-to` lines could send a
# credential or aim a probe elsewhere) or a proxy (`--noproxy '*'`, since a proxy's connect is
# not the device's): it measures what this device itself reaches.
#
# Inputs, each refused by name when missing:
#   OUTSIDE_ORIGIN        the chosen path's origin, https://<host>[:<port>], the host a name, an
#                         IPv4 address or a bracketed IPv6 address.
#   OUTSIDE_PATH_TARGETS  a file of `name host port` lines, one per internal target (the hub, the
#                         listeners, the secrets broker, the other Dispatch origin). `#` starts a
#                         comment, so the address a name stands for goes beside it. `host` is a
#                         name that resolves on this network or a literal address, never a `svc:`
#                         selector, which resolves nowhere.
# OUTSIDE_PATH_ALLOW_HTTP=1 lets an http origin through, and only http://127.0.0.1, for this
# script's own tests. OUTSIDE_PATH_CLASSIFY="<exit> <namelookup> <connect>" runs the classifier
# alone on one recorded probe, prints its class and exits 0.
#
# It prints the origin, this device's name and the UTC time, a line for the origin's positive
# control and one per target, then its verdict:
#   PASS     exit 0: Dispatch answered at the origin, the probe connected to the origin, and every
#            target was unreachable.
#   FAIL     exit 1: a target was reached, or the origin served a read with no credential.
#   UNKNOWN  exit 2: anything that proved nothing, which is never a pass: an input refused, curl
#            missing or too old, the origin not answering as Dispatch, the probe unable to connect
#            to the origin itself, a target that did not resolve or that no rule classifies, or an
#            empty target list.
#
# Every host and port is probed by one command and judged by whether TCP connected, not by curl's
# exit code alone: every target this check exists for (NATS, an HTTP listener, a TLS port) accepts
# and then holds the connection waiting for the client, so a reached target ends at --max-time
# with exit 28, the same exit a blackholed address gives. See `classify`.
#
# Usage: OUTSIDE_ORIGIN=https://dispatch-outside.example OUTSIDE_PATH_TARGETS=targets.txt \
#          bash outside-path-reach.sh
set -u

# unknown MESSAGE: the verdict for anything that proved nothing.
unknown() {
  printf 'UNKNOWN: %s\n' "$1"
  exit 2
}

# The checks of one field each, written with `case` alone: bash 3.2's `=~` quotes differently.
is_count() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }
is_seconds() { case $1 in '' | *[!0-9.]* | .* | *. | *.*.*) return 1 ;; esac; }
is_port() {
  case $1 in '' | *[!0-9]* | 0*) return 1 ;; esac
  [ "${#1}" -le 5 ] && [ "$1" -le 65535 ]
}

# pop: moves the first line of `rest` into `line`, splitting a text already in memory with no
# `read`, so no temporary file and no read of a file can fail half way.
newline=$'\n'
pop() {
  line=${rest%%"$newline"*}
  case $rest in
    *"$newline"*) rest=${rest#*"$newline"} ;;
    *) rest='' ;;
  esac
}

# address HOST PORT: `host:port`, an IPv6 host in brackets.
address() {
  case $1 in
    *:*) printf '[%s]:%s' "$1" "$2" ;;
    *) printf '%s:%s' "$1" "$2" ;;
  esac
}

# classify EXIT NAMELOOKUP CONNECT: prints the class of one probe, from curl's exit code and its
# time_namelookup and time_connect, each 0.000000 when that step never completed. Bash has no
# floating point, so a time is non-zero when it holds a digit 1 to 9; LC_ALL=C on the probe keeps
# the decimal point a point. In this order:
#   REACHED                        time_connect non-zero, whatever the exit code;
#   unresolved (lookup timed out)  exit 28 with both times zero: the name lookup never answered;
#   unreachable                    exit 7 (refused), or exit 28 with the lookup non-zero;
#   unresolved                     exit 6: the name does not resolve on this network;
#   UNKNOWN (...)                  anything else, an empty or malformed value included.
classify() {
  local code=${1-} lookup=${2-} connect=${3-}
  if is_count "$code" && is_seconds "$lookup" && is_seconds "$connect"; then
    case $connect in *[1-9]*)
      echo REACHED
      return
      ;;
    esac
    case $code:$lookup in
      28:*[1-9]* | 7:*)
        echo unreachable
        return
        ;;
      28:*)
        echo 'unresolved (lookup timed out)'
        return
        ;;
      6:*)
        echo unresolved
        return
        ;;
    esac
  fi
  printf 'UNKNOWN (curl exit %s, time_namelookup %s, time_connect %s)\n' "$code" "$lookup" "$connect"
}

if [ -n "${OUTSIDE_PATH_CLASSIFY+set}" ]; then
  set -f
  # shellcheck disable=SC2086 # split on purpose: the three fields of one recorded probe
  set -- $OUTSIDE_PATH_CLASSIFY
  set +f
  [ "$#" -le 3 ] || unknown "OUTSIDE_PATH_CLASSIFY holds more than '<exit> <namelookup> <connect>': $OUTSIDE_PATH_CLASSIFY"
  classify "${1-}" "${2-}" "${3-}"
  exit 0
fi

[ -n "${OUTSIDE_ORIGIN:-}" ] ||
  unknown "OUTSIDE_ORIGIN is unset: set it to the origin the path reaches Dispatch at, https://<host>[:<port>]"
[ -n "${OUTSIDE_PATH_TARGETS:-}" ] ||
  unknown "OUTSIDE_PATH_TARGETS is unset: set it to the file of 'name host port' lines naming every internal target"

origin=${OUTSIDE_ORIGIN%/}
case $origin in
  https://*)
    authority=${origin#https://}
    default_port=443
    ;;
  http://*)
    [ "${OUTSIDE_PATH_ALLOW_HTTP:-}" = 1 ] ||
      unknown "OUTSIDE_ORIGIN must be https, not $origin: http is for this script's own tests, with OUTSIDE_PATH_ALLOW_HTTP=1"
    authority=${origin#http://}
    default_port=80
    ;;
  *) unknown "OUTSIDE_ORIGIN must be an https origin, https://<host>[:<port>], not $origin" ;;
esac
# The authority alone, no user, path, query or fragment: a host name or IPv4 address and an
# optional port, or a bracketed IPv6 address and an optional port.
case $authority in
  \[*\] | \[*\]:*)
    origin_host=${authority#\[}
    origin_host=${origin_host%%\]*}
    origin_port=${authority#*\]}
    origin_port=${origin_port#:}
    case $origin_host in '' | *[!0-9A-Fa-f:.]*) origin_host='' ;; esac
    case $authority in *\]) origin_port=$default_port ;; esac
    ;;
  '' | *[!A-Za-z0-9.:-]* | *:*:*) origin_host='' ;;
  *:*)
    origin_host=${authority%%:*}
    origin_port=${authority#*:}
    ;;
  *)
    origin_host=$authority
    origin_port=$default_port
    ;;
esac
if [ -z "$origin_host" ] || ! is_port "$origin_port"; then
  unknown "OUTSIDE_ORIGIN must be an origin alone, https://<host>[:<port>] with a port of 1 to 65535 and no user, path, query or fragment, not $origin"
fi
case $origin in http://*)
  [ "$origin_host" = 127.0.0.1 ] ||
    unknown "OUTSIDE_ORIGIN may be http only at 127.0.0.1, for this script's own tests, not $origin"
  ;;
esac

# The target list, read whole before anything goes on the network: a file it cannot read whole is
# a refusal, never a target left out. A read error ends a `read` loop exactly as the end of the
# file does, and `$(<file)` and curl's file:// both pass one over in silence; only `read` says so,
# on stderr. So each line comes back marked `L` and bash's own words come back unmarked, and any
# unmarked line refuses the run. Row 0 is the origin itself, the positive control.
targets=$OUTSIDE_PATH_TARGETS
row=''
rest=$({ while IFS= read -r row || [ -n "$row" ]; do
  printf 'L%s\n' "$row"
  row=''
done <"$targets"; } 2>&1) || rest="$rest${newline}could not open it"
hosts[0]=$origin_host
ports[0]=$origin_port
count=0
number=0
while [ -n "$rest" ]; do
  pop
  case $line in
    L*) ;;
    *) unknown "OUTSIDE_PATH_TARGETS names $targets, which this device could not read whole: $line" ;;
  esac
  number=$((number + 1))
  row=${line#L}
  row=${row%%#*}
  row=${row%$'\r'}
  set -f
  # shellcheck disable=SC2086 # split on purpose: the fields of one target line
  set -- $row
  set +f
  [ "$#" -eq 0 ] && continue
  [ "$#" -eq 3 ] || unknown "line $number of $targets is not 'name host port': $row"
  case $2 in svc:*)
    unknown "target $1: $2 is a Tailscale service selector, which resolves nowhere; write a name that resolves on this network, or the address it stands for"
    ;;
  esac
  case $2 in *[!A-Za-z0-9.:-]*)
    unknown "target $1: $2 is neither a host name nor an address"
    ;;
  esac
  is_port "$3" || unknown "target $1: $3 is not a TCP port of 1 to 65535"
  count=$((count + 1))
  names[count]=$1
  hosts[count]=$2
  ports[count]=$3
done
[ "$count" -gt 0 ] || unknown "$targets lists no target: nothing was tried, so nothing is proved"

command -v curl >/dev/null 2>&1 || unknown "curl missing or too old: no curl on PATH; install curl 7.40 or later"
version=$(curl -q --version </dev/null 2>/dev/null)
set -f
# shellcheck disable=SC2086 # split on purpose: `curl <version> (<platform>) ...`
set -- $version
set +f
version=${2-}
major=${version%%.*}
minor=${version#*.}
minor=${minor%%.*}
if ! { is_count "$major" && is_count "$minor" && [ $((10#$major * 1000 + 10#$minor)) -ge 7040 ]; }; then
  unknown "curl missing or too old: this curl is '${version}'; install curl 7.40 or later"
fi

now=''
if command -v date >/dev/null 2>&1; then
  now=$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)
fi
printf 'outside-path-reach: origin %s\n' "$origin"
printf 'outside-path-reach: device %s\n' "${HOSTNAME:-unknown}"
printf 'outside-path-reach: time %s\n' "${now:-unknown (no date command answered)}"
printf 'outside-path-reach: curl %s\n' "$version"
printf 'outside-path-reach: targets %s (%s)\n' "$targets" "$count"

# status PATH: the HTTP status the origin answers PATH with, no credential sent; 000 when none.
status() {
  local answer
  answer=$(curl -q --noproxy '*' -sS -o /dev/null --connect-timeout 3 --max-time 10 \
    -w '%{http_code}' "$origin$1" </dev/null 2>/dev/null)
  printf '%s' "${answer:-000}"
}

# Dispatch answers: its health route with 200, and a read with no credential with 401, which shows
# the path ends at Dispatch and Dispatch is the boundary.
health=$(status /healthz)
projects=$(status /api/v1/projects)
printf 'dispatch: GET /healthz %s, GET /api/v1/projects with no credential %s\n' "$health" "$projects"
[ "$projects" != 200 ] || {
  printf 'FAIL: %s served a read with no credential\n' "$origin"
  exit 1
}
[ "$health" = 200 ] && [ "$projects" = 401 ] ||
  unknown "$origin did not answer as Dispatch; reached nothing, so proved nothing"

# probe INDEX: one probe of row INDEX, printed as `INDEX EXIT NAMELOOKUP CONNECT` on one line.
probe() {
  local times code
  times=$(LC_ALL=C curl -q --noproxy '*' -sS -o /dev/null --connect-timeout 3 --max-time 5 \
    -w '%{time_namelookup} %{time_connect}' "telnet://$(address "${hosts[$1]}" "${ports[$1]}")" \
    </dev/null 2>/dev/null)
  code=$?
  printf '%s %s %s\n' "$1" "$code" "$times"
}

# Every row probed at once: a probe that reaches a holding listener takes its whole --max-time,
# and one after another would take that per target. Each job prints its one short line in one
# write, so the lines do not mix, and a row whose line never came reads UNKNOWN.
results=$(
  index=0
  while [ "$index" -le "$count" ]; do
    probe "$index" &
    index=$((index + 1))
  done
  wait
)
rest=$results
while [ -n "$rest" ]; do
  pop
  set -f
  # shellcheck disable=SC2086 # split on purpose: `INDEX EXIT NAMELOOKUP CONNECT`
  set -- $line
  set +f
  is_count "${1-}" || continue
  codes[$1]=${2-}
  lookups[$1]=${3-}
  connects[$1]=${4-}
done

# report LABEL INDEX: prints the probe's line and sets `class`.
report() {
  class=$(classify "${codes[$2]-}" "${lookups[$2]-}" "${connects[$2]-}")
  case $class in
    UNKNOWN*) printf '%s %s %s\n' "$1" "$(address "${hosts[$2]}" "${ports[$2]}")" "$class" ;;
    *)
      printf '%s %s %s; curl exit %s, time_namelookup %s, time_connect %s\n' "$1" \
        "$(address "${hosts[$2]}" "${ports[$2]}")" "$class" "${codes[$2]}" "${lookups[$2]}" \
        "${connects[$2]}"
      ;;
  esac
}

report 'control origin' 0
control=$class
reached=''
unclear=''
index=1
while [ "$index" -le "$count" ]; do
  report "target ${names[index]}" "$index"
  case $class in
    REACHED) reached="$reached ${names[index]}" ;;
    unreachable) ;;
    *) unclear="$unclear ${names[index]}" ;;
  esac
  index=$((index + 1))
done

# A reached target is a failure whatever else the run saw: a completed connect is evidence, while
# every other way a run can come short only fails to look.
[ -z "$reached" ] || {
  printf 'FAIL: reached%s\n' "$reached"
  exit 1
}
[ "$control" = REACHED ] || unknown "the probe could not connect to the origin itself"
[ -z "$unclear" ] || unknown "${unclear# } unresolved or unknown"
printf 'PASS: every target is unreachable from this device, and %s answered as Dispatch\n' "$origin"
