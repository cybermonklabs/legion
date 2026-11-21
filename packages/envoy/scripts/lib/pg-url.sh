# shellcheck shell=bash
# packages/envoy/scripts/lib/pg-url.sh
#
# How these scripts hand a Postgres URL to psql, pg_restore or another libpq client: the URL
# without its password as the client's argument, and the password in PGPASSWORD, set for that one
# command alone, never exported. A process's arguments are its /proc/<pid>/cmdline, which every
# user of the machine reads (ps prints them) for as long as it runs; its environment is
# /proc/<pid>/environ, which only its own user and root read.
#
# Sourced, never run. A caller uses two functions:
#
#   pg_url_run URL COMMAND [ARG...]
#       Runs COMMAND with the URL without its password as its first argument, then each ARG, and
#       the password in PGPASSWORD for that command alone. For a URL that holds no password it
#       runs COMMAND with the URL as it is and leaves PGPASSWORD as the caller's environment has
#       it. COMMAND may be a function of the caller's: bash passes PGPASSWORD on to every command
#       that function runs. It returns COMMAND's status.
#   pg_url_without_password URL
#       Prints the URL without its password, for a banner or a log line.
#
# The URL is split as libpq splits it (fe-connect.c, conninfo_uri_parse_options), so the password
# taken out is the one psql would have read. The same rules are the Python
# scripts/search-measure/pg.py and the TypeScript packages/dispatch/e2e/pg-url.ts, which change
# with this one:
#   - the credentials run from after postgres:// or postgresql:// to the first @ before the first
#     /, and the password is what follows their first :; libpq reads an empty one as none, so a
#     URL whose user has no password, or that names no user, holds none in its credentials;
#   - a password= in the query string, its name percent-decoded as libpq decodes it, is a password
#     too: it is taken out with its & and passed the same way, and the last one wins over every
#     other, as it does in libpq;
#   - the password is percent-decoded as libpq decodes it: %XX is the byte XX;
#   - every other byte of the URL (the user, the hosts, the ports, the database and every other
#     query parameter) is kept as it is, and a URL that holds no password is kept whole.
# It refuses what libpq refuses in a password (a % that starts no %XX escape, and %00, which libpq
# quotes in its error), a host in [ ] that libpq cannot read (no ], or a ] followed by anything but
# :, /, ? or ,), a raw @ in the hosts (the text past a password's raw @, which libpq would print in
# its host error), an @ between the first / and the ? (a password's raw /, which leaves libpq no
# credentials, so it reads the user as the host and the password's head as the port, and prints
# that in its error), and anything but a postgres:// or postgresql:// URL, since a key=value
# connection string can hold a password this file does not take out. A refusal names what it
# refused on standard error, prints nothing of the URL, runs nothing and returns 2.
#
# Under xtrace (set -x) both functions below turn tracing off as they start and back on as they
# return, after COMMAND, keeping COMMAND's status: bash traces every expansion of the password,
# and a PGPASSWORD=... prefix with its value, and a trace goes wherever the script's standard
# error goes. Every call line above them that holds the URL is traced before they run, URL and
# all: the caller's, and each wrapper's on the way (dev-stack.sh's admin_psql, then its
# server_psql). No function can take those back.

# pg_url_run URL COMMAND [ARG...]: see the header.
pg_url_run() {
  local pg_url_xtrace="" pg_url_status=0 pg_url_split_url pg_url_split_password pg_url_split_given
  [[ $- != *x* ]] || { pg_url_xtrace=1 && { set +x; } 2>/dev/null; }
  if pg_url_split_into "$1"; then
    shift
    if [ "$pg_url_split_given" = true ]; then
      PGPASSWORD="$pg_url_split_password" "$1" "$pg_url_split_url" "${@:2}" || pg_url_status=$?
    else
      "$1" "$pg_url_split_url" "${@:2}" || pg_url_status=$?
    fi
  else
    pg_url_status=$?
  fi
  [ -z "$pg_url_xtrace" ] || set -x
  return "$pg_url_status"
}

# pg_url_without_password URL: see the header.
pg_url_without_password() {
  local pg_url_xtrace="" pg_url_status=0 pg_url_split_url pg_url_split_password pg_url_split_given
  [[ $- != *x* ]] || { pg_url_xtrace=1 && { set +x; } 2>/dev/null; }
  if pg_url_split_into "$1"; then
    printf '%s\n' "$pg_url_split_url"
  else
    pg_url_status=$?
  fi
  [ -z "$pg_url_xtrace" ] || set -x
  return "$pg_url_status"
}

# pg_url_split_into URL sets its callers' pg_url_split_url (the URL without its password),
# pg_url_split_password (the password, decoded) and pg_url_split_given (true when the URL holds a
# password; an empty password= in the query string is one, which libpq takes over PGPASSWORD).
# The two functions above declare the three local and call it with tracing already off. Bytes are
# bytes here (LC_ALL=C), as they are to libpq: in a multibyte locale one character can hide an @.
pg_url_split_into() {
  local LC_ALL=C
  local url="$1" scheme rest credentials user="" location tail head query param key kept="" removed=false
  pg_url_split_password=""
  pg_url_split_given=false
  case "$url" in
    postgres://* | postgresql://*) ;;
    *)
      pg_url_refuse "it is not a postgres:// or postgresql:// URL"
      return 2
      ;;
  esac
  scheme="${url%%://*}://"
  rest="${url#*://}"
  # An @ between the first / and the ? is a password's raw / (libpq's credentials run only to the
  # first /), and libpq would read the user as a host, the password's head as a port, and print it.
  local before_query="${rest%%\?*}"
  if [[ "$before_query" == */* && "${before_query#*/}" == *@* ]]; then
    pg_url_refuse "an @ sits between the first / and the ?"
    return 2
  fi
  # location is everything past the credentials: the hosts, the database and the query string.
  location="$rest"
  if [[ "${rest%%/*}" == *@* ]]; then
    credentials="${rest%%@*}"
    location="${rest#*@}"
    user="$credentials@"
    if [[ "$credentials" == *:* && -n "${credentials#*:}" ]]; then
      if ! pg_url_decode pg_url_split_password "${credentials#*:}"; then
        pg_url_refuse "its password holds a % that starts no %XX escape, or %00"
        return 2
      fi
      user="${credentials%%:*}@"
      pg_url_split_given=true
    fi
  fi
  # libpq takes the credentials to the first @ and reads a second one as part of a host, whose
  # error then prints it: a password's @ that is not written %40 leaves its tail there.
  if [[ "${location%%[/?]*}" == *@* ]]; then
    pg_url_refuse "a host holds a raw @"
    return 2
  fi
  # The query string starts at the first ? past the hosts, read as libpq reads them: a host in
  # [ ] runs to its ], every other host and each port runs to a :, /, ? or the , before the next
  # host, and the database runs to a ?. tail is what is left past each step.
  tail="$location"
  while :; do
    if [[ "$tail" == \[* ]]; then
      if [[ "$tail" != *]* ]]; then
        pg_url_refuse "a host's [ has no ]"
        return 2
      fi
      tail="${tail#*]}"
    else
      tail="${tail#"${tail%%[:/?,]*}"}"
    fi
    if [[ "$tail" == :* ]]; then
      tail="${tail#:}"
      tail="${tail#"${tail%%[/?,]*}"}"
    fi
    if [[ "$tail" == ,* ]]; then
      tail="${tail#,}"
      continue
    fi
    break
  done
  case "$tail" in
    "" | /* | \?*) ;;
    *)
      pg_url_refuse "a host's ] is followed by something other than :, /, ? or ,"
      return 2
      ;;
  esac
  if [[ "$tail" == /* ]]; then
    tail="${tail#"${tail%%\?*}"}"
  fi
  head="${location%"$tail"}"
  query="${tail#\?}"
  while [ -n "$query" ]; do
    param="${query%%&*}"
    if [[ "$query" == *\&* ]]; then query="${query#*&}"; else query=""; fi
    if [[ "$param" == *=* ]] && pg_url_decode key "${param%%=*}" && [ "$key" = password ]; then
      if ! pg_url_decode pg_url_split_password "${param#*=}"; then
        pg_url_refuse "its query string's password holds a % that starts no %XX escape, or %00"
        return 2
      fi
      pg_url_split_given=true
      removed=true
    else
      kept="${kept:+$kept&}$param"
    fi
  done
  if [ "$removed" = true ]; then
    tail="${kept:+?$kept}"
  fi
  pg_url_split_url="$scheme$user$head$tail"
}

# pg_url_decode VARIABLE TEXT sets VARIABLE to TEXT percent-decoded as libpq decodes it, and
# fails, setting nothing, on a % that starts no %XX escape and on %00.
pg_url_decode() {
  local pg_url_decode_in="$2" pg_url_decode_out="" pg_url_decode_byte
  while [[ "$pg_url_decode_in" == *%* ]]; do
    pg_url_decode_out+="${pg_url_decode_in%%\%*}"
    pg_url_decode_in="${pg_url_decode_in#*%}"
    if [[ "${pg_url_decode_in:0:2}" != [0123456789ABCDEFabcdef][0123456789ABCDEFabcdef] ||
      "${pg_url_decode_in:0:2}" == 00 ]]; then
      return 1
    fi
    printf -v pg_url_decode_byte '%b' "\\x${pg_url_decode_in:0:2}"
    pg_url_decode_out+="$pg_url_decode_byte"
    pg_url_decode_in="${pg_url_decode_in:2}"
  done
  printf -v "$1" '%s' "$pg_url_decode_out$pg_url_decode_in"
}

# pg_url_refuse REASON names a refused URL's fault on standard error, and nothing of the URL.
pg_url_refuse() {
  printf '%s: refused a Postgres URL: %s (the URL is not printed)\n' "${0##*/}" "$1" >&2
}
