#!/usr/bin/env bash
# The HTTPS refusal check of the outside edge: it asks the edge's load balancer, at the edge's one
# address, for each path and Host the edge must not forward, and reads whether one of our routes
# took the request. What it measures is the answer each request gets through that address: its
# status, and the response header X-Outside-Edge-Backend, which every route of the edge sets to
# the backend it reached, so an answer without it came through none of them. A TCP check such as
# outside-path-reach.sh sees which ports answer and cannot see this: an address that answers on
# 443 forwards by path and by Host there, and a request it refuses connects as well as one it
# forwards.
#
# It is one file that sources nothing, and needs bash (3.2, as macOS ships it, or later), curl
# 8.4.0 or later (from October 2023), mktemp, and sha256sum or shasum to hash each answer's body.
# bash, mktemp and a hasher are on a stock macOS or Linux; an older curl refuses the run by name,
# since before 8.4.0 `--max-filesize` does not bound a body whose length the answer did not give
# first, as curl's manual says. No curl it runs reads the terminal (stdin is /dev/null), the
# device's curl configuration (`-q` ignores ~/.curlrc, whose `header`, `proxy`, `resolve` or
# `connect-to` lines could change a probe), a proxy (`--noproxy '*'`) or a pattern of several URLs
# in its URL (`-g`).
#
# Inputs, each refused by name when missing:
#   OUTSIDE_EDGE_ADDRESS   the edge's IPv4 address, written as four numbers of 0 to 255 with no
#                          leading zero. Every probe goes to it on 443, wherever the names it
#                          sends resolve on this machine (`curl --resolve`).
#   OUTSIDE_EDGE_ROUTED    a file of `name method sni host path backend` lines, the positive
#                          controls, one or more per backend: each a request one of our routes
#                          takes and marks with `X-Outside-Edge-Backend: <backend>`. A backend is
#                          never `none`, the word a probe line prints for an answer with no marker.
#   OUTSIDE_EDGE_REFUSALS  a file of `name method sni host path [405:marker]` lines, each a
#                          request the edge must refuse. The optional sixth field is for a method
#                          the edge refuses on a path it forwards on purpose: `405:` and the marker
#                          of the backend that refuses it, letters, digits, `.`, `_` and `-` but
#                          not `none`, such as `405:broker` for a GET on the broker's token path.
#                          No route of the edge matches on method (its Gateway class supports
#                          none), so that GET reaches the broker, and the broker's own 405,
#                          marked, is its refusal. 405 is the status RFC 9110 gives a method its
#                          target does not support; another status there would let a list take an
#                          answer the edge forwarded, such as a backend's own 401 or 404, for a
#                          refusal.
# Both lists split their fields on spaces and tabs; a line whose first field starts with `#` is a
# comment, and a blank line is skipped. Every byte must be printable ASCII or a tab, but a CR that
# comes directly before an LF (CRLF line endings). Each line's name is its own, across both lists,
# so a verdict that names a line names one. `method` is GET, POST, PUT, PATCH, DELETE or OPTIONS:
# not HEAD, for which `curl -X HEAD` waits for a body that never comes. `sni` and `host` are host
# names, never an address: a route's hostname cannot be an address, so no route could be planted
# for one, and a refusal line that no planted route can reach has no control. `path` starts with
# `/` and holds URL characters alone (RFC 3986's path and query), so no `#`; and no dot segment
# (`/./`, `/../`, or `/.` or `/..` at its end), no `//` and no encoded slash (`%2f`, `%2F`): curl
# rewrites a dot segment before it sends the path (`/a/../b` goes out as `/b`), and the Gateway
# API refuses a route whose path holds any of them, so none could be planted for such a line.
# OUTSIDE_EDGE_CONNECT_SECONDS and OUTSIDE_EDGE_READ_SECONDS bound each probe, as curl's
# --connect-timeout and --max-time: 3 and 10 s when unset or empty. Each must be a whole number of
# seconds from 2 to 999999, in digits, as outside-path-reach.sh's own bounds are, for the reasons
# its header gives; any other value refuses the run, naming the setting.
# OUTSIDE_EDGE_ALLOW_HTTP=1 lets OUTSIDE_EDGE_ADDRESS be http://127.0.0.1:<port>, and nothing
# else, for this script's own tests: each probe then goes over HTTP to that port.
#
# Each probe is one HTTP/1.1 request, which keeps the Host it sends literal:
#   curl --http1.1 --resolve <sni>:443:<address> -H 'Host: <host>' -X <method> [-d ''] \
#     -D <headers> -o <body> -w '%{http_code}' --connect-timeout 3 --max-time 10 \
#     --max-filesize 1048576 https://<sni><path>
# with an empty body (`-d ''`) for POST, PUT and PATCH. The certificate is verified against the SNI.
# curl stops reading a body at 1 MiB, with exit 63, whether or not the answer gave its length first
# (curl 8.4.0 and later), so no answer fills the disk the run writes to.
# curl's `-D` writes every header block of the answer, an informational 1xx before the final one,
# and the status it prints is the final answer's. A marker is read from every block: an answer
# names a backend when its final block carries a marker and every marker of every block is that
# backend, so neither an earlier block nor a second marker can stand for the answer.
#
# The routed controls are read first. Each must answer naming its backend, whatever its status;
# one that does not shows this run cannot see a route of this edge, and the run stops at UNKNOWN
# before it reads any refusal line. Then each refusal line of five fields first has its unrouted
# control sent: the line's own method, SNI and path, with the Host
# unrouted.outside-edge-refusals.invalid, which no route can name (RFC 6761 keeps `.invalid` out of
# the DNS). Its answer is the edge's own 404 for that path, and it must come whole, 404 with no
# marker. Then the line itself is
#   refused  when its answer came whole and is the one the line names: for a line of five fields,
#            404, no marker in any block and the same body as its unrouted control's; for a line
#            of six, exactly 405, naming exactly the sixth field's marker;
#   REACHED  when any other answer came: for a line of five fields, a marker of any value in any
#            block or a status other than 404;
#   UNKNOWN  when no status came (a connect or TLS failure, a timeout before the answer), when
#            the named answer came and curl did not read it whole, its body past 1 MiB included,
#            or when a line of five fields came 404 with no marker and its unrouted control did
#            not answer the edge's own 404, or answered one with another body. A route that
#            forwards without a marker to a backend that answers 404 gives another body; so may
#            an edge whose own 404 differs by Host, and the run cannot tell the two apart, so it
#            reads UNKNOWN, never refused.
# Each probe prints its status (000 where none came), every marker of its answer in order and
# separated by `,` (`none` where it had none), and the SHA-256 of its body (`none` where no answer
# came), so two runs can be compared, and curl's exit code and first message where curl did not
# exit 0; an unrouted control prints `unrouted <name>` the same way. Then the verdict:
#   PASS     exit 0: every routed control showed its marker and every refusal line was refused.
#   FAIL     exit 1: a refusal line was REACHED, whatever else the run could not read, a signal
#            that stopped it afterwards included: an answer is evidence, where a probe that came
#            short only failed to look.
#   UNKNOWN  exit 2: anything that proved nothing, which is never a pass: an input refused, a
#            tool missing or too old, an empty or unreadable list, a routed control whose marker
#            was not visible, a refusal line whose answer could not be read as a refusal or a
#            reach, or a run that a HUP, INT or TERM stopped before its verdict and before any
#            line was REACHED.
#
# A HUP, INT or TERM ends the run at a verdict, never at bash's own exit with nothing printed. From
# the first line of any verdict on, through the exit's cleanup, the three are ignored, so a second
# signal can neither print a second verdict nor cut the cleanup short; the exit kills every job
# the run started, a list's reader included, and removes the directory the answers went to. While
# a list is read, a signal ends the run at once, sent to the script's own process alone or to its
# whole process group: a job of its own reads each list, and the script waits for that job in
# `wait`, which a trapped signal interrupts on every bash (bash holds a trap until a read in the
# shell itself or in a command substitution ends, and bash 3.2 restarts an interrupted open of a
# named pipe). While a probe runs, bash runs the trap once that probe's curl has returned: a
# signal sent to the script's process alone ends the run within OUTSIDE_EDGE_READ_SECONDS, and one
# sent to its process group, as a terminal's Ctrl-C is, ends that curl too, and the run at once.
#
# Usage: OUTSIDE_EDGE_ADDRESS=192.0.2.10 OUTSIDE_EDGE_ROUTED=routed.txt \
#          OUTSIDE_EDGE_REFUSALS=refusals.txt bash outside-edge-refusals.sh

# A shell other than bash stops here, before it reads a line it would take another way.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash outside-edge-refusals.sh'
  exit 2
fi
set -u
# Every pattern below matches bytes, whatever locale the machine runs in.
LC_ALL=C
# bash's `read` takes an inherited TMOUT as its timeout, which would cut a list short.
unset TMOUT 2>/dev/null
# The refusal lines reached so far, set here so no variable of the environment stands for one.
reached=''

# verdict CODE TEXT: prints TEXT, the run's one verdict, and exits with CODE. A HUP, INT or TERM is
# ignored from its first line on, through the exit's cleanup, so a second signal can neither print
# a second verdict nor cut the cleanup short.
verdict() {
  trap '' HUP INT TERM
  printf '%s\n' "$2"
  exit "$1"
}
# unknown MESSAGE: the verdict for anything that proved nothing.
unknown() { verdict 2 "UNKNOWN: $1"; }
# stopped: the verdict of a run a HUP, INT or TERM stopped: FAIL where a refusal line was reached
# before it, since an answer is evidence whatever came after it, and UNKNOWN otherwise.
# shellcheck disable=SC2329 # the trap below invokes it
stopped() {
  [ -z "$reached" ] || verdict 1 "FAIL: reached$reached, before a signal stopped the run"
  unknown "the run was stopped by a signal before its verdict"
}
# Set before anything else can wait: the header says when a HUP, INT or TERM ends the run.
trap stopped HUP INT TERM

is_count() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }
is_port() {
  case $1 in '' | *[!0-9]* | 0*) return 1 ;; esac
  [ "${#1}" -le 5 ] && [ "$1" -le 65535 ]
}

# is_ipv4 TEXT: TEXT is an IPv4 address in its one canonical form, four numbers of 0 to 255 with
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

# is_host_name TEXT: TEXT is a host name: letters, digits and `-` in labels joined by single dots,
# with no dot at either end, no `-` first, and a last label that is neither a number nor a 0x hex
# number, which curl reads as part of an address.
is_host_name() {
  local last=${1##*.}
  case $1 in '' | *[!A-Za-z0-9.-]* | .* | *. | *..* | -*) return 1 ;; esac
  case $last in *[!0-9]*) ;; *) return 1 ;; esac
  case $last in 0[xX]*) return 1 ;; esac
}

# check_seconds NAME VALUE: refuses the run, naming NAME, unless VALUE is a whole number of seconds
# from 2 to 999999, in digits (the header says why).
check_seconds() {
  local whole=${2#"${2%%[!0]*}"}
  is_count "$2" && [ -n "$whole" ] && [ "$whole" != 1 ] && [ "${#whole}" -le 6 ] && return
  unknown "$1 must be a whole number of seconds from 2 to 999999, not $2"
}

# pop: moves the first line of `rest` into `line`, splitting a text already in memory with no
# `read`, so no read of a file can fail half way.
newline=$'\n'
pop() {
  line=${rest%%"$newline"*}
  case $rest in
    *"$newline"*) rest=${rest#*"$newline"} ;;
    *) rest='' ;;
  esac
}

# outside: one byte that is not printable ASCII or a tab. LF never reaches a line, since lines
# are split on it; NUL never reaches one either, since the read stops at it.
outside=$'[!\t -~]'
# url_outside: one byte a path may not hold as written: anything but RFC 3986's unreserved
# characters, its sub-delimiters, `:`, `@`, `/`, `%` and `?`.
url_outside="[!A-Za-z0-9._~!\$&'()*+,;=:@/%?-]"

# is_path TEXT: TEXT starts with `/` and holds no byte of `url_outside`.
is_path() {
  case $1 in /*) ;; *) return 1 ;; esac
  # shellcheck disable=SC2254 # $url_outside is a bracket expression on purpose, so it stays unquoted
  case $1 in *$url_outside*) return 1 ;; esac
}

# is_expected TEXT: TEXT is a refusal line's expected answer, `405:<marker>`: the method refusal of
# a backend, and a marker of letters, digits, `.`, `_` and `-`, never empty and never `none`.
is_expected() {
  case $1 in 405:?*) ;; *) return 1 ;; esac
  case ${1#405:} in none | *[!A-Za-z0-9._-]*) return 1 ;; esac
}

# byte_hex BYTE: sets `hex` to the two hexadecimal digits of BYTE, one byte other than NUL, as
# outside-path-reach.sh's byte_hex does (its comment says why the mask).
byte_hex() {
  local n
  printf -v n '%d' "'$1"
  printf -v hex '%02X' $((n & 255))
}

# read_whole SETTING FILE: sets `text` to the whole of FILE, the list SETTING names, read once
# before anything goes on the network. A job of its own reads it, and this shell waits for that
# job in `wait`, which a trapped signal interrupts on every bash (the header says why); the EXIT
# trap kills a job a signal left reading. One `read -d ''` reads the whole list: it stops at a NUL
# byte and succeeds only when it found one, fails with status 1 at the end of the list, and says
# so on stderr only for a read error. Any other status is a read that stopped before the end,
# such as a timeout. The job writes the read's stderr, its status and the text to files of
# `work`, and exits 0 only once all three are written. A job that wrote no status could not open
# the file, and bash says why on stderr; words of bash's own on the read's stderr refuse the run.
read_whole() {
  local base="$work/$1" status said=''
  {
    IFS= read -r -d '' text 2>"$base.said"
    printf '%s' "$?" >"$base.status" && printf '%s' "$text" >"$base.text"
  } <"$2" &
  wait "$!"
  status=$?
  if [ "$status" -ne 0 ]; then
    [ -f "$base.status" ] || unknown "$1 names $2, which could not be read: could not open it"
    unknown "$1 names $2, which could not be read whole: what was read could not be written under $work"
  fi
  IFS= read -r said <"$base.said"
  [ -z "$said" ] || unknown "$1 names $2, which could not be read whole: $said"
  IFS= read -r status <"$base.status"
  text=''
  IFS= read -r -d '' text <"$base.text"
  case $status in
    0)
      local lfs=${text//[!$newline]/}
      unknown "line $((${#lfs} + 1)) of $2 holds byte 0x00, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings"
      ;;
    1) ;;
    *)
      unknown "$1 names $2, which could not be read whole: the read stopped with status $status before its end"
      ;;
  esac
}

# read_list SETTING FILE KIND: reads FILE, the list SETTING names, and appends each of its lines to
# the probe table, after row `total`, refusing the run at the first line it cannot take. KIND is
# `routed`, whose lines have six fields, the last the backend, or `refusals`, whose lines have five,
# or six with the expected answer last.
read_list() {
  local setting=$1 file=$2 kind=$3 shape='name method sni host path backend' number=0 ended row
  local prefix
  [ "$kind" = routed ] || shape='name method sni host path [405:marker]'
  read_whole "$setting" "$file"
  rest=$text
  while [ -n "$rest" ]; do
    # ended: the line ends in an LF, so a CR at its end comes directly before one.
    case $rest in *"$newline"*) ended=yes ;; *) ended=no ;; esac
    pop
    number=$((number + 1))
    row=$line
    [ "$ended" = no ] || row=${row%$'\r'}
    # shellcheck disable=SC2254 # $outside is a bracket expression on purpose, so it stays unquoted
    case $row in *$outside*)
      # shellcheck disable=SC2295 # the same bracket expression, as a pattern on purpose
      prefix=${row%%$outside*}
      byte_hex "${row:${#prefix}:1}"
      unknown "line $number of $file holds byte 0x$hex, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings"
      ;;
    esac
    set -f
    # shellcheck disable=SC2086 # split on purpose: the fields of one line
    set -- $row
    set +f
    [ "$#" -eq 0 ] && continue
    case $1 in '#'*) continue ;; esac
    case $kind:$# in routed:6 | refusals:5 | refusals:6) ;; *)
      unknown "line $number of $file is not '$shape': $row"
      ;;
    esac
    case $1 in '' | *[!A-Za-z0-9._-]*)
      unknown "line $number of $file: the name $1 is not letters, digits, '.', '_' and '-'"
      ;;
    esac
    case " $taken " in *" $1 "*)
      unknown "line $number of $file: the name $1 is another line's; give each line a name of its own"
      ;;
    esac
    taken="$taken $1"
    case $2 in GET | POST | PUT | PATCH | DELETE | OPTIONS) ;; *)
      unknown "line $number of $file: the method $2 is not one of GET, POST, PUT, PATCH, DELETE or OPTIONS"
      ;;
    esac
    is_host_name "$3" || unknown "line $number of $file: the sni $3 is not a host name"
    is_host_name "$4" ||
      unknown "line $number of $file: the host $4 is not a host name; no route can name an address, so none could be planted for it"
    is_path "$5" ||
      unknown "line $number of $file: the path $5 is not an absolute path of URL characters"
    case $5 in */./* | */../* | */. | */.. | *//* | *%2[fF]*)
      unknown "line $number of $file: the path $5 holds a dot segment, '//' or an encoded slash, which curl rewrites or no route can name, so none could be planted for it"
      ;;
    esac
    if [ "$kind" = routed ]; then
      case $6 in *[!A-Za-z0-9._-]*)
        unknown "line $number of $file: the backend $6 is not letters, digits, '.', '_' and '-'"
        ;;
      esac
      [ "$6" != none ] ||
        unknown "line $number of $file: the backend none is the word a probe line prints for an answer with no marker; name the backend another way"
    elif [ "$#" -eq 6 ] && ! is_expected "$6"; then
      unknown "line $number of $file: the expected answer $6 is not '405:<marker>', the method refusal of a backend whose marker is letters, digits, '.', '_' and '-' and not none"
    fi
    total=$((total + 1))
    names[total]=$1
    methods[total]=$2
    snis[total]=$3
    hosts[total]=$4
    paths[total]=$5
    backends[total]=''
    expects[total]=''
    if [ "$kind" = routed ]; then backends[total]=$6; else expects[total]=${6-}; fi
  done
}

[ -n "${OUTSIDE_EDGE_ADDRESS:-}" ] ||
  unknown "OUTSIDE_EDGE_ADDRESS is unset: set it to the edge's IPv4 address"
[ -n "${OUTSIDE_EDGE_ROUTED:-}" ] ||
  unknown "OUTSIDE_EDGE_ROUTED is unset: set it to the file of 'name method sni host path backend' lines, one routed control per backend"
[ -n "${OUTSIDE_EDGE_REFUSALS:-}" ] ||
  unknown "OUTSIDE_EDGE_REFUSALS is unset: set it to the file of 'name method sni host path [405:marker]' lines the edge must refuse"

connect=${OUTSIDE_EDGE_CONNECT_SECONDS:-3}
max_time=${OUTSIDE_EDGE_READ_SECONDS:-10}
check_seconds OUTSIDE_EDGE_CONNECT_SECONDS "$connect"
check_seconds OUTSIDE_EDGE_READ_SECONDS "$max_time"

given=$OUTSIDE_EDGE_ADDRESS
case $given in
  http://*)
    [ "${OUTSIDE_EDGE_ALLOW_HTTP:-}" = 1 ] ||
      unknown "OUTSIDE_EDGE_ADDRESS must be the edge's IPv4 address, not $given: http is for this script's own tests, with OUTSIDE_EDGE_ALLOW_HTTP=1"
    port=${given#http://127.0.0.1:}
    if [ "$port" = "$given" ] || ! is_port "$port"; then
      unknown "OUTSIDE_EDGE_ADDRESS may be http only as http://127.0.0.1:<port>, for this script's own tests, not $given"
    fi
    scheme=http
    address=127.0.0.1
    authority_port=:$port
    ;;
  *)
    is_ipv4 "$given" ||
      unknown "OUTSIDE_EDGE_ADDRESS must be an IPv4 address written as four numbers of 0 to 255 with no leading zero, not $given"
    scheme=https
    address=$given
    port=443
    authority_port=''
    ;;
esac

command -v curl >/dev/null 2>&1 || unknown "curl missing or too old: no curl on PATH; install curl 8.4.0 or later"
version=$(curl -q --version </dev/null 2>/dev/null)
set -f
# shellcheck disable=SC2086 # split on purpose: `curl <version> (<platform>) ...`
set -- $version
set +f
version=${2-}
major=${version%%.*}
minor=${version#*.}
minor=${minor%%.*}
if ! { is_count "$major" && is_count "$minor" && [ $((10#$major * 1000 + 10#$minor)) -ge 8004 ]; }; then
  unknown "curl missing or too old: this curl is '${version}'; install curl 8.4.0 or later"
fi
command -v mktemp >/dev/null 2>&1 ||
  unknown "mktemp missing: no mktemp on PATH, which makes the directory the answers are written to"
if command -v sha256sum >/dev/null 2>&1; then
  hasher=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  hasher=shasum
else
  unknown "sha256sum and shasum missing: neither is on PATH, and one hashes each answer's body"
fi

# The directory each list's reader writes what it read to, and each probe its headers, body and
# curl's stderr, files of its own; removed when the run exits, once every job the run started, a
# reader a signal left reading included, is killed. `jobs -p` lists a reader from the moment it
# is forked, so a signal before the script has its process id leaves none behind. The three
# signals are ignored first, so a second one cannot cut the cleanup short.
work=$(mktemp -d "${TMPDIR:-/tmp}/outside-edge-refusals.XXXXXX" 2>/dev/null) && [ -d "$work" ] ||
  unknown "mktemp could not make a directory under ${TMPDIR:-/tmp} for the answers"
trap 'trap "" HUP INT TERM; left=$(jobs -p); [ -z "$left" ] || { kill -KILL $left; wait $left; } 2>/dev/null; rm -rf "$work"' EXIT

# The probe table: the routed controls in rows 1 to `routed`, then the refusal lines to `total`.
total=0
taken=''
read_list OUTSIDE_EDGE_ROUTED "$OUTSIDE_EDGE_ROUTED" routed
routed=$total
[ "$routed" -gt 0 ] ||
  unknown "$OUTSIDE_EDGE_ROUTED lists no routed control: no marker can be seen, so no refusal can be read"
read_list OUTSIDE_EDGE_REFUSALS "$OUTSIDE_EDGE_REFUSALS" refusals
[ "$total" -gt "$routed" ] ||
  unknown "$OUTSIDE_EDGE_REFUSALS lists no refusal line: nothing was tried, so nothing is proved"

now=''
if command -v date >/dev/null 2>&1; then
  now=$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)
fi
# No line names this machine's host name: the output goes to logs anyone may read, such as a
# public CI log.
printf 'outside-edge-refusals: address %s\n' "$given"
printf 'outside-edge-refusals: time %s\n' "${now:-unknown (no date command answered)}"
printf 'outside-edge-refusals: curl %s\n' "$version"
printf 'outside-edge-refusals: routed %s (%s)\n' "$OUTSIDE_EDGE_ROUTED" "$routed"
printf 'outside-edge-refusals: refusals %s (%s)\n' "$OUTSIDE_EDGE_REFUSALS" $((total - routed))

# digest_of FILE: sets `digest` to the SHA-256 of FILE, `unreadable` where the hash failed.
digest_of() {
  local out
  if [ "$hasher" = sha256sum ]; then
    out=$(sha256sum <"$1" 2>/dev/null)
  else
    out=$(shasum -a 256 <"$1" 2>/dev/null)
  fi
  out=${out%% *}
  case $out in *[!0-9a-f]* | '') digest=unreadable ;; *) digest=$out ;; esac
  [ "${#out}" -eq 64 ] || digest=unreadable
}

# The Host of each refusal line's unrouted control: a name no route can name, since RFC 6761 keeps
# `.invalid` out of the DNS.
unrouted_host=unrouted.outside-edge-refusals.invalid

# probe ROW [HOST]: one request for row ROW of the table, as the header writes it, with HOST as
# its Host where it is given (an unrouted control), the row's own otherwise. Sets `status`, to 000
# where no status came; `marked`, yes or no: whether any header block carried a marker; `seen`,
# every marker value of every block as it came, and `final`, those of the final block, each one
# per line; `shown`, every marker value in order, each with any byte outside letters, digits, `.`,
# `_` and `-` written `?` and `''` for an empty one, separated by `,`, and `none` where there was
# none; `digest`, the body's SHA-256, of an empty body where an answer came with none, and `none`
# where no answer came; `code`, curl's exit code; and `said`, curl's first line on stderr.
probe() {
  local row=$1 host=${2-} file header value part
  file=$work/$1${host:+.unrouted}
  [ -n "$host" ] || host=${hosts[row]}
  local headers="$file.headers" body="$file.body" errors="$file.errors"
  set -- -q --noproxy '*' -g -sS --http1.1 --resolve "${snis[row]}:$port:$address" \
    -H "Host: $host" -X "${methods[row]}"
  case ${methods[row]} in POST | PUT | PATCH) set -- "$@" -d '' ;; esac
  set -- "$@" -D "$headers" -o "$body" -w '%{http_code}' --connect-timeout "$connect" \
    --max-time "$max_time" --max-filesize 1048576 "$scheme://${snis[row]}$authority_port${paths[row]}"
  status=$(LC_ALL=C curl "$@" </dev/null 2>"$errors")
  code=$?
  case $status in [0-9][0-9][0-9]) ;; *) status=000 ;; esac
  marked=no
  seen=''
  final=''
  shown=none
  if [ -f "$headers" ]; then
    while IFS= read -r header || [ -n "$header" ]; do
      header=${header%$'\r'}
      case $header in
        HTTP/*) final='' ;;
        [Xx]-[Oo][Uu][Tt][Ss][Ii][Dd][Ee]-[Ee][Dd][Gg][Ee]-[Bb][Aa][Cc][Kk][Ee][Nn][Dd]:*)
          value=${header#*:}
          value=${value#"${value%%[![:blank:]]*}"}
          value=${value%"${value##*[![:blank:]]}"}
          marked=yes
          seen="$seen$value$newline"
          final="$final$value$newline"
          part=${value//[!A-Za-z0-9._-]/?}
          [ -n "$part" ] || part="''"
          if [ "$shown" = none ]; then shown=$part; else shown="$shown,$part"; fi
          ;;
      esac
    done <"$headers"
  fi
  if [ -f "$body" ]; then
    digest_of "$body"
  elif [ "$status" != 000 ]; then
    digest_of /dev/null
  else
    digest=none
  fi
  said=''
  if [ "$code" -ne 0 ] && [ -f "$errors" ]; then
    IFS= read -r said <"$errors"
    said=${said%$'\r'}
    said=${said//[![:print:]]/?}
  fi
}

# answer_names MARKER: the answer of the probe just made names MARKER: its final header block
# carried a marker, and every marker of every block is MARKER.
answer_names() {
  local left=$seen each
  [ -n "$final" ] || return 1
  while [ -n "$left" ]; do
    each=${left%%"$newline"*}
    left=${left#*"$newline"}
    [ "$each" = "$1" ] || return 1
  done
}

# report LABEL ROW [NOTE]: prints the line of the probe just made of row ROW, NOTE after it.
report() {
  local tail=''
  [ "$code" -eq 0 ] || tail="; curl exit $code${said:+: $said}"
  printf '%s %s %s %s body-sha256 %s%s%s\n' "$1" "${names[$2]}" "$status" "$shown" "$digest" "$tail" "${3-}"
}

# The routed controls, every one of them, before any refusal line: a control whose answer did not
# name its backend shows that a refusal's 404 with no marker would say nothing.
missing=''
row=1
while [ "$row" -le "$routed" ]; do
  probe "$row"
  report control "$row"
  if ! answer_names "${backends[row]}"; then
    [ -n "$missing" ] || missing=${names[row]}
  fi
  row=$((row + 1))
done
[ -z "$missing" ] || unknown "the marker of $missing is not visible, so no refusal can be read"

# unread: the lines whose answer came short; unclear: the lines of five fields answered 404 with
# no marker whose body could not be told from a forward, as the header says.
unread=''
unclear=''
while [ "$row" -le "$total" ]; do
  # own: the body's SHA-256 of the edge's own 404 for this line, from its unrouted control; empty
  # for a line of six fields, and where the control did not answer 404 with no marker, read whole.
  own=''
  if [ -z "${expects[row]}" ]; then
    probe "$row" "$unrouted_host"
    report unrouted "$row"
    if [ "$marked" = no ] && [ "$status" = 404 ] && [ "$code" -eq 0 ]; then own=$digest; fi
  fi
  probe "$row"
  if [ -n "${expects[row]}" ]; then
    if [ "$status" = "${expects[row]%%:*}" ] && answer_names "${expects[row]#*:}"; then
      if [ "$code" -eq 0 ]; then kind=refused; else kind=unread; fi
    elif [ "$marked" = yes ] || [ "$status" != 000 ]; then
      kind=REACHED
    else
      kind=unread
    fi
  elif [ "$marked" = yes ] || { [ "$status" != 000 ] && [ "$status" != 404 ]; }; then
    kind=REACHED
  elif [ "$status" = 000 ] || [ "$code" -ne 0 ]; then
    kind=unread
  elif [ -z "$own" ]; then
    kind="; its unrouted control did not answer 404 with no marker, read whole"
  elif [ "$digest" != "$own" ]; then
    kind="; another body than its unrouted control's 404"
  else
    kind=refused
  fi
  case $kind in
    refused) report refused "$row" ;;
    REACHED)
      report REACHED "$row"
      reached="$reached ${names[row]}"
      ;;
    unread)
      report UNKNOWN "$row"
      unread="$unread ${names[row]}"
      ;;
    *)
      report UNKNOWN "$row" "$kind"
      unclear="$unclear ${names[row]}"
      ;;
  esac
  row=$((row + 1))
done

# A reached line is a failure whatever else the run saw: an answer is evidence, while a probe
# that came short only failed to look.
[ -z "$reached" ] || verdict 1 "FAIL: reached$reached"
if [ -n "$unread$unclear" ]; then
  message=''
  [ -z "$unread" ] || message="no answer could be read whole for$unread"
  [ -z "$unclear" ] ||
    message="${message:+$message; }no 404 with no marker could be told from a forward for$unclear: its body is not the edge's own 404 for its path"
  unknown "$message"
fi
verdict 0 "PASS: every one of the $((total - routed)) refusal lines was refused (the edge's own 404 with no marker, or the answer its line names), after each of the $routed routed controls showed its marker"
