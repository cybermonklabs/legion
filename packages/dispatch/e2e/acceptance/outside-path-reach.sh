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
#                         comment. `host` is a name or a literal address: an IPv4 address written
#                         as four numbers of 0 to 255 with no leading zero, or an IPv6 address;
#                         never a `svc:` selector, which resolves nowhere. The list must hold each
#                         of our services' addresses on the path as lines of their own, not names
#                         alone: a name line proves only where that name resolves on this device,
#                         and a name that resolves elsewhere there, or nowhere, says nothing of the
#                         address behind it. A list of names alone draws a warning.
# OUTSIDE_PATH_ALLOW_HTTP=1 lets an http origin through, and only http://127.0.0.1, for this
# script's own tests. OUTSIDE_PATH_CLASSIFY="<exit> <namelookup> <connect>" runs the classifier
# alone on one recorded probe, prints its class and exits 0.
#
# It prints the origin, this device's name and the UTC time, a line for the origin's positive
# control and one per target, then its verdict:
#   PASS     exit 0: Dispatch answered at the origin (/healthz 200, and /api/v1/projects with no
#            credential 401 with Dispatch's own NO_IDENTITY refusal), the probe connected to the
#            origin, and every target was unreachable.
#   FAIL     exit 1: a target was reached, or the origin served a read with no credential.
#   UNKNOWN  exit 2: anything that proved nothing, which is never a pass: an input refused, curl
#            missing or too old, the origin not answering as Dispatch, the probe unable to connect
#            to the origin itself, a target that did not resolve or that no rule classifies, or an
#            empty target list.
#
# Every host and port is probed three times at once by one command and judged by whether TCP
# connected, not by curl's exit code alone: every target this check exists for (NATS, an HTTP
# listener, a TLS port) accepts and then holds the connection waiting for the client, so a reached
# target ends at --max-time with exit 28, the same exit a blackholed address gives. A target is
# REACHED when any of its three probes connected, unreachable only when all three were, and
# otherwise takes the class of its first probe that was neither; three probes, not one, so a
# network that loses packets does not hide a target. See `classify` for the classes.
#
# Usage: OUTSIDE_ORIGIN=https://dispatch-outside.example OUTSIDE_PATH_TARGETS=targets.txt \
#          bash outside-path-reach.sh

# A shell other than bash stops here, before it reads a line it would take another way.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-path-reach.sh'
  exit 2
fi
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

# is_ipv4 HOST: HOST is an IPv4 address in its one canonical form, four numbers of 0 to 255 with
# no leading zero, which is how curl reads it too (curl reads 010 as octal 8).
is_ipv4() {
  local rest=$1. part count=0
  while [ -n "$rest" ]; do
    part=${rest%%.*}
    rest=${rest#*.}
    case $part in '' | *[!0-9]* | 0?*) return 1 ;; esac
    [ "${#part}" -le 3 ] && [ "$part" -le 255 ] || return 1
    count=$((count + 1))
  done
  [ "$count" -eq 4 ]
}

# host_ok HOST: HOST is a name, an IPv6 address, or an IPv4 address in its canonical form. As
# Dispatch's own origin rule has it, a host whose last label is a number or a 0x hex number is an
# address, so it must be a canonical one: 127.1, 0x7f000001 and 172.020.000.021 each name another
# host than they seem to.
host_ok() {
  case $1 in *:*) return 0 ;; esac
  case ${1##*.} in
    *[!0-9]*) case ${1##*.} in 0[xX]*) ;; *) return 0 ;; esac ;;
  esac
  is_ipv4 "$1"
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
# time_namelookup and time_connect, each 0.000000 when that step never completed. The connect
# timeout, 3 s, covers the lookup and the connect together. Bash has no floating point, so a time
# is non-zero when it holds a digit 1 to 9, and a second or more when its whole part does; LC_ALL=C
# on the probe keeps the decimal point a point. In this order:
#   REACHED                        time_connect non-zero, whatever the exit code;
#   unreachable                    exit 7 (refused);
#   unresolved (lookup timed out)  exit 28 with both times zero: the name lookup never answered;
#   unreachable                    exit 28 with a lookup of under a second, leaving the connect two
#                                  seconds or more to time out in;
#   UNKNOWN (the lookup took ...)  exit 28 with a lookup of a second or more: the connect had too
#                                  little time to show the target does not answer;
#   unresolved                     exit 6: the name does not resolve on this network;
#   UNKNOWN (curl exit ...)        anything else, an empty or malformed value included.
classify() {
  local code=${1-} lookup=${2-} connect=${3-}
  if is_count "$code" && is_seconds "$lookup" && is_seconds "$connect"; then
    case $connect in *[1-9]*)
      echo REACHED
      return
      ;;
    esac
    case $code in
      7)
        echo unreachable
        return
        ;;
      28)
        case $lookup in
          *[1-9]*) ;;
          *)
            echo 'unresolved (lookup timed out)'
            return
            ;;
        esac
        case ${lookup%%.*} in
          *[1-9]*)
            printf 'UNKNOWN (the lookup took %s s of the 3 s to connect, too long to read a timed-out connect as unreachable; curl exit 28, time_connect %s)\n' "$lookup" "$connect"
            ;;
          *) echo unreachable ;;
        esac
        return
        ;;
      6)
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
host_ok "$origin_host" ||
  unknown "OUTSIDE_ORIGIN host $origin_host is not an IPv4 address written as four numbers of 0 to 255 with no leading zero"
case $origin in http://*)
  [ "$origin_host" = 127.0.0.1 ] ||
    unknown "OUTSIDE_ORIGIN may be http only at 127.0.0.1, for this script's own tests, not $origin"
  ;;
esac

# The target list, read whole before anything goes on the network: a file it cannot read whole is
# a refusal, never a target left out. A read error ends a `read` loop exactly as the end of the
# file does, and `$(<file)` and curl's file:// both pass one over in silence; only `read` says so,
# on stderr. So each line comes back marked `L` and bash's own words come back unmarked, and any
# unmarked line refuses the run. A UTF-8 byte-order mark before the first line is dropped. Row 0
# is the origin itself, the positive control.
targets=$OUTSIDE_PATH_TARGETS
bom=$'\xef\xbb\xbf'
row=''
rest=$({ while IFS= read -r row || [ -n "$row" ]; do
  printf 'L%s\n' "$row"
  row=''
done <"$targets"; } 2>&1) || rest="$rest${newline}could not open it"
hosts[0]=$origin_host
ports[0]=$origin_port
count=0
addresses=0
number=0
while [ -n "$rest" ]; do
  pop
  case $line in
    L*) ;;
    *) unknown "OUTSIDE_PATH_TARGETS names $targets, which this device could not read whole: $line" ;;
  esac
  number=$((number + 1))
  row=${line#L}
  [ "$number" -gt 1 ] || row=${row#"$bom"}
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
  host_ok "$2" ||
    unknown "target $1: $2 is not an IPv4 address written as four numbers of 0 to 255 with no leading zero"
  is_port "$3" || unknown "target $1: $3 is not a TCP port of 1 to 65535"
  count=$((count + 1))
  names[count]=$1
  hosts[count]=$2
  ports[count]=$3
  { case $2 in *:*) ;; *) is_ipv4 "$2" ;; esac; } && addresses=$((addresses + 1))
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
[ "$addresses" -gt 0 ] ||
  echo "outside-path-reach: warning: every target is a name, and a name proves only where it resolves on this device; list each of our services' addresses on the path as lines of their own"

# fetch PATH: what the origin answers PATH with, no credential sent: its body, then its three-digit
# status, 000 when none came.
fetch() {
  local answer
  answer=$(curl -q --noproxy '*' -sS -o - --connect-timeout 3 --max-time 10 \
    -w '%{http_code}' "$origin$1" </dev/null 2>/dev/null)
  [ "${#answer}" -ge 3 ] || answer=000
  printf '%s' "$answer"
}

# Dispatch answers: its health route with 200, and a read with no credential with 401 and its own
# NO_IDENTITY refusal, which shows the path ends at Dispatch and Dispatch is the boundary.
health=$(fetch /healthz)
health=${health#"${health%???}"}
projects=$(fetch /api/v1/projects)
refusal=none
case ${projects%???} in *'"code":"NO_IDENTITY"'* | *'"code": "NO_IDENTITY"'*) refusal=NO_IDENTITY ;; esac
projects=${projects#"${projects%???}"}
printf 'dispatch: GET /healthz %s, GET /api/v1/projects with no credential %s, code %s\n' \
  "$health" "$projects" "$refusal"
[ "$projects" != 200 ] || {
  printf 'FAIL: %s served a read with no credential\n' "$origin"
  exit 1
}
[ "$health" = 200 ] && [ "$projects" = 401 ] && [ "$refusal" = NO_IDENTITY ] ||
  unknown "$origin did not answer as Dispatch; reached nothing, so proved nothing"

# probe INDEX ATTEMPT: one probe of row INDEX, printed as `INDEX ATTEMPT EXIT NAMELOOKUP CONNECT`
# on one line.
probe() {
  local times code
  times=$(LC_ALL=C curl -q --noproxy '*' -sS -o /dev/null --connect-timeout 3 --max-time 5 \
    -w '%{time_namelookup} %{time_connect}' "telnet://$(address "${hosts[$1]}" "${ports[$1]}")" \
    </dev/null 2>/dev/null)
  code=$?
  printf '%s %s %s %s\n' "$1" "$2" "$code" "$times"
}

# Every row probed three times, all at once: a probe that reaches a holding listener takes its
# whole --max-time, and one after another would take that per probe. Each job prints its one short
# line in one write, so the lines do not mix, and a probe whose line never came reads UNKNOWN.
results=$(
  index=0
  while [ "$index" -le "$count" ]; do
    probe "$index" 0 &
    probe "$index" 1 &
    probe "$index" 2 &
    index=$((index + 1))
  done
  wait
)
rest=$results
while [ -n "$rest" ]; do
  pop
  set -f
  # shellcheck disable=SC2086 # split on purpose: `INDEX ATTEMPT EXIT NAMELOOKUP CONNECT`
  set -- $line
  set +f
  is_count "${1-}" && is_count "${2-}" && [ "$2" -lt 3 ] || continue
  key=$(($1 * 3 + $2))
  codes[key]=${3-}
  lookups[key]=${4-}
  connects[key]=${5-}
done

# report LABEL INDEX: judges row INDEX from its three probes, prints its line and sets `class`.
report() {
  local attempt=0 key each decisive=0 probes=''
  class=''
  while [ "$attempt" -lt 3 ]; do
    key=$(($2 * 3 + attempt))
    each=$(classify "${codes[key]-}" "${lookups[key]-}" "${connects[key]-}")
    probes="$probes, ${each%% *}"
    if [ "$each" = REACHED ]; then
      [ "$class" = REACHED ] || {
        class=REACHED
        decisive=$key
      }
    elif [ "$each" != unreachable ]; then
      case $class in '' | unreachable)
        class=$each
        decisive=$key
        ;;
      esac
    elif [ -z "$class" ]; then
      class=$each
      decisive=$key
    fi
    attempt=$((attempt + 1))
  done
  case $class in
    UNKNOWN*)
      printf '%s %s %s; probes: %s\n' "$1" "$(address "${hosts[$2]}" "${ports[$2]}")" "$class" \
        "${probes#, }"
      ;;
    *)
      printf '%s %s %s; curl exit %s, time_namelookup %s, time_connect %s; probes: %s\n' "$1" \
        "$(address "${hosts[$2]}" "${ports[$2]}")" "$class" "${codes[decisive]}" \
        "${lookups[decisive]}" "${connects[decisive]}" "${probes#, }"
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
