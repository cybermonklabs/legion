#!/usr/bin/env bash
# The driver of the check c-outside-path-reach (dispatch://INFRA-3862): a person from outside runs
# it on their own device, over the path we give them, to measure which of our listed addresses that
# device reaches. What it measures is TCP connects from that device to the listed addresses and
# ports, nothing more: it cannot see a service the origin itself forwards by path or by Host on its
# own host and port, and it must run on the device itself, not in a VM, a container or a remote
# shell, whose network is another one.
#
# It is handed to that person as this one file with its target list, so it sources nothing and
# needs only bash (3.2, as macOS ships it, or later), curl 7.40 or later and sleep, all three on a
# stock macOS or Linux: no GNU `timeout`, which stock macOS lacks, since curl's own
# --connect-timeout and --max-time bound every probe. No curl it runs reads the terminal (stdin is
# /dev/null), the device's curl configuration (`-q` ignores ~/.curlrc, whose `header`, `proxy`,
# `resolve` or `connect-to` lines could send a credential or aim a probe elsewhere) or a proxy
# (`--noproxy '*'`, since a proxy's connect is not the device's), and none reads `[`, `]`, `{` or
# `}` in its URL as a pattern of several URLs (`-g`): it measures what this device itself reaches.
# A run takes about 29 seconds.
#
# Inputs, each refused by name when missing:
#   OUTSIDE_ORIGIN        the chosen path's origin, https://<host>[:<port>], the host a name, an
#                         IPv4 address or a bracketed IPv6 address, on the rule a target's host
#                         follows.
#   OUTSIDE_PATH_TARGETS  a file, or a pipe, of `name host port` lines, one per internal target
#                         (the hub, the listeners, the secrets broker, the other Dispatch origin).
#                         `#` starts a comment. `host` is a name or a literal address: an IPv4
#                         address written as four numbers of 0 to 255 with no leading zero, or an
#                         IPv6 address of hexadecimal digits and colons alone, not IPv4-mapped
#                         however written (its text must not start with ffff: once its leading
#                         zeros and colons are dropped, as ::ffff:a00:1 and 0:0::ffff:a00:1 do:
#                         write 10.0.0.1); never
#                         a `svc:` selector, which resolves nowhere. The list is ASCII, comments
#                         included: every byte must be printable ASCII, a tab or LF, but a CR that
#                         comes directly before an LF (CRLF line endings) and a UTF-8 byte-order
#                         mark at offset 0, which are dropped before anything parses the list. Any
#                         other byte refuses the run, naming the byte and its offset, since `read`
#                         splits only on LF and a reader may split on that byte. The list must
#                         hold each of our services' addresses on the path as lines of their own,
#                         not names alone: a name line proves only where that name resolves on this
#                         device, and a name that resolves elsewhere there, or nowhere, says nothing
#                         of the address behind it. A list of names alone draws a warning.
# OUTSIDE_PATH_ALLOW_HTTP=1 lets an http origin through, and only http://127.0.0.1, for this
# script's own tests. OUTSIDE_PATH_CLASSIFY="<exit> <namelookup> <connect>" runs the classifier
# alone on one recorded probe and prints its class; five of them separated by `;` are judged as
# one target's five rounds, in order, and the target's class is printed. Either way it exits 4,
# never PASS's 0, and refuses an empty value with exit 2.
# OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS and OUTSIDE_PATH_ORIGIN_READ_SECONDS bound each of the two
# reads of the origin, as curl's --connect-timeout and --max-time: 3 and 10 s when unset or empty.
# Each must be a positive number of seconds, digits with an optional fraction (0.5, 30), and any
# other value refuses the run, naming the setting. They do not bound the probes, whose 3 s to
# connect and 5 s in all the classifier's rules count on.
#
# It prints the origin, this device's name and the UTC time, a line for the origin's positive
# control and one per target, then its verdict:
#   PASS     exit 0: Dispatch answered at the origin (/healthz 200, and /api/v1/projects with no
#            credential 401 with Dispatch's own NO_IDENTITY refusal), the probe connected to the
#            origin in every round, and every target was unreachable.
#   FAIL     exit 1: a target was reached, or the origin served a read with no credential.
#   UNKNOWN  exit 2: anything that proved nothing, which is never a pass: an input refused, curl
#            or sleep missing or curl too old, the origin not answering as Dispatch, the probe
#            unable to connect to the origin in some round, a target that did not resolve or that
#            no rule classifies, an empty target list, or rounds that sleep could not spread apart.
#
# Every host and port is probed in five rounds, started 0, 6, 12, 18 and 24 s into the probing, all
# hosts of a round at once, by one command, and each probe is judged by whether TCP connected, not
# by curl's exit code alone: every target this check exists for (NATS, an HTTP listener, a TLS
# port) accepts and then holds the connection waiting for the client, so a reached target ends at
# --max-time with exit 28, the same exit a blackholed address gives. A target's round counts only
# when the probe connected to the origin in that same round: a round in which the device reached
# not even the origin proves nothing of a target, which reads `uncounted` there unless it
# connected. A target is REACHED when any of its rounds connected, unreachable only when all five
# counted and were, and otherwise takes the class of its first round that was neither. Five
# rounds, so a network that loses packets does not hide a
# target; 6 s apart, so neither does a tunnel whose first handshake packet was lost: WireGuard
# sends it again only after its RekeyTimeout of 5 s plus up to 334 ms, and probes started at once
# would all fall inside that. Heavy loss can still miss all five, though none of our runs did: with
# 70% of the packets to a reachable target dropped at random, 240 runs (bash 3.2 and 5.2) printed no
# PASS, about a third of the rounds missing, where three rounds printed PASS in 8 of 240.
# See `classify` for the classes and `judge` for a target's rounds.
#
# Usage: OUTSIDE_ORIGIN=https://dispatch-outside.example OUTSIDE_PATH_TARGETS=targets.txt \
#          [OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS=3] [OUTSIDE_PATH_ORIGIN_READ_SECONDS=10] \
#          bash outside-path-reach.sh

# A shell other than bash stops here, before it reads a line it would take another way.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-path-reach.sh'
  exit 2
fi
set -u
# Every pattern below matches bytes, whatever locale the device runs in.
LC_ALL=C

# rounds: how many rounds of probes each host gets, started 6 s apart (see the header).
rounds=5
# gated: set once the origin's own rounds are read; `judge` then counts a target's round only when
# the origin connected in it (`origin_connected`).
gated=''

# unknown MESSAGE: the verdict for anything that proved nothing.
unknown() {
  printf 'UNKNOWN: %s\n' "$1"
  exit 2
}

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

# host_ok HOST: HOST is a name (a trailing dot allowed), an IPv6 address, or an IPv4 address in
# its canonical form. As Dispatch's own origin rule has it, a host whose last label is a number or
# a 0x hex number is an address, so it must be a canonical one: 127.1, 0x7f000001 and
# 172.020.000.021 each name another host than they seem to.
host_ok() {
  local last=${1%.}
  case $1 in *:*) return 0 ;; esac
  last=${last##*.}
  case $last in
    *[!0-9]*) case $last in 0[xX]*) ;; *) return 0 ;; esac ;;
  esac
  is_ipv4 "$1"
}

# v4_in_v6 HOST: HOST is an IPv6 address that holds a dotted IPv4 part (::127.0.0.1,
# ::ffff:127.0.0.1), or whose text starts with ffff: once its leading zeros and colons are dropped:
# the IPv4-mapped form in any spelling (::ffff:7f00:1, 0:0::ffff:7f00:1, 0000:…:ffff:7f00:0001).
# Each names another host than its IPv4 address does: ::127.0.0.1 is not loopback, and a v6-only
# stack (OpenBSD, or Linux with bindv6only=1) reaches no mapped address, so the target would read
# unreachable while its IPv4 address answers.
v4_in_v6() {
  local rest=$1
  case $rest in *:*.*) return 0 ;; esac
  while :; do
    case $rest in [0:]*) rest=${rest#?} ;; *) break ;; esac
  done
  case $rest in [fF][fF][fF][fF]:*) return 0 ;; esac
  return 1
}

# check_host WHO HOST: refuses HOST, the origin's host or a target's, unless it is a name, an IPv6
# address of hexadecimal digits and colons alone, or an IPv4 address in its canonical form; WHO
# begins each message (`target hub:`, `OUTSIDE_ORIGIN host`). curl reads no such host as a URL
# pattern. A dot in an IPv6 address is a dotted IPv4 part, which `v4_in_v6` refuses first; any dot
# left in a coloned host (`.::1`, `a.b::1`, `10.0.0.1:4222`) is no part of an address at all.
check_host() {
  case $2 in
    *:*)
      case $2 in *[!0-9A-Fa-f:.]*) unknown "$1 $2 is neither a host name nor an address" ;; esac
      ;;
    *[!A-Za-z0-9.-]* | .* | *..*) unknown "$1 $2 is neither a host name nor an address" ;;
  esac
  ! v4_in_v6 "$2" ||
    unknown "$1 $2 is an IPv6 form of an IPv4 address; write the IPv4 address itself"
  case $2 in *:*.* | *.*:*) unknown "$1 $2 is neither a host name nor an address" ;; esac
  host_ok "$2" ||
    unknown "$1 $2 is not an IPv4 address written as four numbers of 0 to 255 with no leading zero"
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

# judge EXIT NAMELOOKUP CONNECT (once per round, one target's rounds in order): sets `class` to
# the target's class and `detail` to what its line says after the address. Once `gated` is set, a
# round in which the origin did not connect counts for nothing but a connect: it reads `uncounted`
# unless the target connected in it. REACHED when any round connected, unreachable only when all
# of them were, and otherwise the class of the first round that was neither; `detail` gives curl's
# answer in the round that decided, then every round's class in order.
judge() {
  local round=0 each code lookup connect decided='' seen='' f1 f2 f3
  class=''
  while [ "$round" -lt "$rounds" ]; do
    code=$1 lookup=$2 connect=$3
    shift 3
    each=$(classify "$code" "$lookup" "$connect")
    if [ -n "$gated" ] && [ "$each" != REACHED ] && [ -z "${origin_connected[round]-}" ]; then
      each="uncounted (round $((round + 1)) of $rounds: the probe could not connect to the origin in that round, so it proves nothing of this target)"
    fi
    seen="$seen, ${each%% *}"
    if [ "$each" = REACHED ]; then
      [ "$class" = REACHED ] || {
        class=REACHED
        decided="$code $lookup $connect"
      }
    elif [ "$each" != unreachable ]; then
      case $class in '' | unreachable)
        class=$each
        decided="$code $lookup $connect"
        ;;
      esac
    elif [ -z "$class" ]; then
      class=$each
      decided="$code $lookup $connect"
    fi
    round=$((round + 1))
  done
  case $class in
    UNKNOWN*) detail="$class; probes: ${seen#, }" ;;
    *)
      fields "$decided"
      detail="$class; curl exit $f1, time_namelookup $f2, time_connect $f3; probes: ${seen#, }"
      ;;
  esac
}

# fields TEXT: sets f1, f2 and f3 to the fields of one recorded probe; fails on more than three.
fields() {
  set -f
  # shellcheck disable=SC2086 # split on purpose: the fields of one recorded probe
  set -- $1
  set +f
  [ "$#" -le 3 ] || return 1
  f1=${1-} f2=${2-} f3=${3-}
}

if [ -n "${OUTSIDE_PATH_CLASSIFY+set}" ]; then
  recorded=$OUTSIDE_PATH_CLASSIFY
  case $recorded in *[![:space:]]*) ;; *)
    unknown "OUTSIDE_PATH_CLASSIFY is set but empty: give it '<exit> <namelookup> <connect>', or $rounds of them separated by ';'"
    ;;
  esac
  malformed="OUTSIDE_PATH_CLASSIFY is neither '<exit> <namelookup> <connect>' nor $rounds of them separated by ';': $recorded"
  rest=$recorded
  parts=0
  set --
  while :; do
    fields "${rest%%;*}" || unknown "$malformed"
    set -- "$@" "$f1" "$f2" "$f3"
    parts=$((parts + 1))
    case $rest in
      *';'*) rest=${rest#*;} ;;
      *) break ;;
    esac
  done
  if [ "$parts" -eq 1 ]; then
    classify "$1" "$2" "$3"
  elif [ "$parts" -eq "$rounds" ]; then
    judge "$@"
    printf '%s\n' "$detail"
  else
    unknown "$malformed"
  fi
  exit 4
fi

[ -n "${OUTSIDE_ORIGIN:-}" ] ||
  unknown "OUTSIDE_ORIGIN is unset: set it to the origin the path reaches Dispatch at, https://<host>[:<port>]"
[ -n "${OUTSIDE_PATH_TARGETS:-}" ] ||
  unknown "OUTSIDE_PATH_TARGETS is unset: set it to the file of 'name host port' lines naming every internal target"

# check_seconds NAME VALUE: refuses the run, naming NAME, unless VALUE is a positive number of
# seconds, digits with an optional fraction, which curl's --connect-timeout and --max-time take.
check_seconds() {
  case $2 in
    '' | *[!0-9.]* | .* | *. | *.*.*)
      unknown "$1 must be a positive number of seconds, digits with an optional fraction, not $2"
      ;;
  esac
  case $2 in *[1-9]*) ;; *)
    unknown "$1 must be a positive number of seconds, digits with an optional fraction, not $2"
    ;;
  esac
}
origin_connect=${OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS:-3}
origin_read=${OUTSIDE_PATH_ORIGIN_READ_SECONDS:-10}
check_seconds OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS "$origin_connect"
check_seconds OUTSIDE_PATH_ORIGIN_READ_SECONDS "$origin_read"

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
  \[*:*\] | \[*:*\]:*)
    origin_host=${authority#\[}
    origin_host=${origin_host%%\]*}
    origin_port=${authority#*\]}
    origin_port=${origin_port#:}
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
check_host "OUTSIDE_ORIGIN host" "$origin_host"
case $origin in http://*)
  [ "$origin_host" = 127.0.0.1 ] ||
    unknown "OUTSIDE_ORIGIN may be http only at 127.0.0.1, for this script's own tests, not $origin"
  ;;
esac

# The target list, read whole and once before anything goes on the network (a pipe can be read
# only once): a list it cannot read whole is a refusal, never a target left out. One `read -d ''`
# reads it all: it stops at a NUL byte and succeeds only when it found one, fails with status 1 at
# the end of the list, and says so on stderr only for a read error, which ends it as the end of the
# list would (`$(<file)` and curl's file:// pass one over in silence). Any other status is a read
# that stopped before the end: a timeout TMOUT sets (an inherited TMOUT=1 cut a piped list short
# and the run passed, so the read unsets it) or a signal. So `listed` holds the read's stderr, then
# an LF, the read's status, an LF and the text: anything before that first LF is bash's own words,
# and refuses the run. Row 0 is the origin itself, the positive control.
targets=$OUTSIDE_PATH_TARGETS
listed=$({
  unset TMOUT 2>/dev/null
  text=''
  IFS= read -r -d '' text 2>&1
  printf '\n%s\n%s.' "$?" "$text"
} <"$targets") ||
  unknown "OUTSIDE_PATH_TARGETS names $targets, which this device could not read whole: could not open it"
listed=${listed%.}
case $listed in "$newline"*) ;; *)
  unknown "OUTSIDE_PATH_TARGETS names $targets, which this device could not read whole: ${listed%%"$newline"*}"
  ;;
esac
listed=${listed#"$newline"}
bom=$'\xef\xbb\xbf'
# outside: one byte that is not printable ASCII or a tab. LF never reaches a row, since rows are
# split on it; NUL never reaches one either, since the read stops at it.
outside=$'[!\t -~]'

# byte_hex BYTE: sets `hex` to the two hexadecimal digits of BYTE, one byte other than NUL. printf
# gives 'c for a byte above 0x7F as a signed char in bash 3.2 (builtins/printf.def, asciicode()),
# -123 for 0x85, and as U+DF85 in bash 5.2 on musl's C locale; each keeps the byte in its low eight
# bits, hence the mask.
byte_hex() {
  local n
  printf -v n '%d' "'$1"
  printf -v hex '%02X' $((n & 255))
}

# refuse_byte LINE OFFSET HEX: the verdict for the byte 0xHEX at OFFSET of the list, counted from
# 0, on its line LINE.
refuse_byte() {
  unknown "line $1 of $targets holds byte 0x$3 at offset $2, which is not printable ASCII, a tab or LF: write the list in ASCII, one target per line, with LF or CRLF line endings"
}

# The read's status: 0 at a NUL byte, with `text` what came before it; 1 at the end of the list,
# with `text` all of it; anything else before the end.
text=${listed#*"$newline"}
case ${listed%%"$newline"*} in
  0)
    lfs=${text//[!$newline]/}
    refuse_byte $((${#lfs} + 1)) "${#text}" 00
    ;;
  1) ;;
  *)
    unknown "OUTSIDE_PATH_TARGETS names $targets, which this device could not read whole: the read stopped with status ${listed%%"$newline"*} before the end of the list"
    ;;
esac
rest=$text
hosts[0]=$origin_host
ports[0]=$origin_port
count=0
addresses=0
number=0
offset=0
while [ -n "$rest" ]; do
  # ended: the line ends in an LF, so a CR at its end comes directly before one.
  case $rest in *"$newline"*) ended=yes ;; *) ended=no ;; esac
  pop
  number=$((number + 1))
  row=$line
  start=$offset
  offset=$((offset + ${#row} + 1))
  # The byte-order mark is dropped at offset 0 alone, and a CR only where an LF follows it at once;
  # any byte left outside printable ASCII and tab refuses the list before any comment is cut.
  if [ "$start" -eq 0 ]; then
    stripped=${row#"$bom"}
    start=$((${#row} - ${#stripped}))
    row=$stripped
  fi
  [ "$ended" = no ] || row=${row%$'\r'}
  # shellcheck disable=SC2254 # $outside is a bracket expression on purpose, so it stays unquoted
  case $row in *$outside*)
    # shellcheck disable=SC2295 # the same bracket expression, as a pattern on purpose
    prefix=${row%%$outside*}
    byte_hex "${row:${#prefix}:1}"
    refuse_byte "$number" $((start + ${#prefix})) "$hex"
    ;;
  esac
  row=${row%%#*}
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
  check_host "target $1:" "$2"
  is_port "$3" || unknown "target $1: $3 is not a TCP port of 1 to 65535"
  count=$((count + 1))
  names[count]=$1
  hosts[count]=$2
  ports[count]=$3
  { case $2 in *:*) ;; *) is_ipv4 "$2" ;; esac; } && addresses=$((addresses + 1))
done
[ "$count" -gt 0 ] || unknown "$targets lists no target: nothing was tried, so nothing is proved"

command -v curl >/dev/null 2>&1 || unknown "curl missing or too old: no curl on PATH; install curl 7.40 or later"
command -v sleep >/dev/null 2>&1 || unknown "sleep missing: no sleep on PATH, which spaces the rounds of probes"
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
  answer=$(curl -q --noproxy '*' -g -sS -o - --connect-timeout "$origin_connect" \
    --max-time "$origin_read" -w '%{http_code}' "$origin$1" </dev/null 2>/dev/null)
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
  times=$(LC_ALL=C curl -q --noproxy '*' -g -sS -o /dev/null --connect-timeout 3 --max-time 5 \
    -w '%{time_namelookup} %{time_connect}' "telnet://$(address "${hosts[$1]}" "${ports[$1]}")" \
    </dev/null 2>/dev/null)
  code=$?
  printf '%s %s %s %s\n' "$1" "$2" "$code" "$times"
}

# Five rounds, 6 s apart, every row probed at once within a round: a probe that reaches a holding
# listener takes its whole --max-time, and one after another would take that per probe. Each job
# prints its one short line in one write, so the lines do not mix, and a probe whose line never
# came reads UNKNOWN. A sleep that fails would start the next round early, so it says so.
results=$(
  round=0
  while [ "$round" -lt "$rounds" ]; do
    if [ "$round" -gt 0 ]; then
      sleep 6 || echo 'rounds not spread'
    fi
    index=0
    while [ "$index" -le "$count" ]; do
      probe "$index" "$round" &
      index=$((index + 1))
    done
    round=$((round + 1))
  done
  wait
)
spread=yes
rest=$results
while [ -n "$rest" ]; do
  pop
  [ "$line" != 'rounds not spread' ] || spread=no
  set -f
  # shellcheck disable=SC2086 # split on purpose: `INDEX ATTEMPT EXIT NAMELOOKUP CONNECT`
  set -- $line
  set +f
  is_count "${1-}" && is_count "${2-}" && [ "$2" -lt "$rounds" ] || continue
  key=$(($1 * rounds + $2))
  codes[key]=${3-}
  lookups[key]=${4-}
  connects[key]=${5-}
done

# report LABEL INDEX: judges row INDEX from its rounds, prints its line and sets `class`.
report() {
  local label=$1 row=$2 round=0 key
  shift 2
  while [ "$round" -lt "$rounds" ]; do
    key=$((row * rounds + round))
    set -- "$@" "${codes[key]-}" "${lookups[key]-}" "${connects[key]-}"
    round=$((round + 1))
  done
  judge "$@"
  printf '%s %s %s\n' "$label" "$(address "${hosts[$row]}" "${ports[$row]}")" "$detail"
}

report 'control origin' 0
control=$class
# The rounds in which the origin connected: only they count for a target that did not connect.
missed='' missed_count=0 round=0
while [ "$round" -lt "$rounds" ]; do
  if [ "$(classify "${codes[round]-}" "${lookups[round]-}" "${connects[round]-}")" = REACHED ]; then
    origin_connected[round]=1
  else
    missed="${missed:+$missed, }$((round + 1))"
    missed_count=$((missed_count + 1))
  fi
  round=$((round + 1))
done
gated=yes
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
[ "$spread" = yes ] || unknown "sleep failed between the rounds of probes, so they were not 6 s apart"
[ "$control" = REACHED ] || unknown "the probe could not connect to the origin itself"
if [ -n "$missed" ]; then
  if [ "$missed_count" -eq 1 ]; then word=round; else word=rounds; fi
  unknown "the probe could not connect to the origin in $word $missed of $rounds, and a round without the origin proves nothing of the targets"
fi
[ -z "$unclear" ] || unknown "${unclear# } unresolved or unknown"
printf 'PASS: every target is unreachable from this device, and %s answered as Dispatch\n' "$origin"
