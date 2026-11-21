#!/usr/bin/env bash
# The driver of the check c-restricted-sweep: a read sweep of a Dispatch deployment as a
# restricted member, over every GET route of the route index and both of Dispatch's sockets, for
# the objects of one project whose access list does not name that member.
#
# It reads the route index the server's route table is held to
# (packages/envoy/internal/dispatch/api/testdata/routes.txt) and, for each GET row:
#   - a row whose path names objects is called with each wildcard filled with an object of the
#     project SWEEP_PROJECT, which a full member's token looked up first (the lookup). The member
#     must be answered 404 or 403, or what a missing object is answered, and never a row of that
#     project;
#   - a row whose path names no object is a list: it is called bare, and once more for each of
#     its query parameters (read from packages/contracts/schemas/dispatch-openapi.json) that
#     names an object of a project, filled with that project's object. A list may answer rows of
#     the member's own projects, never one of SWEEP_PROJECT.
# Two kinds of object no route lists are got another way: a question's run, which the full member
# asks of the project (POST /api/v1/going/requests), and a file of an agent's conversation, which
# SWEEP_CONVERSATION_FILE names. A GET row the sweep cannot fill leaves the run UNKNOWN. A
# repository a row names is one only the project's issues resolve to, never one a project the
# member sees resolves to as well.
# More reads:
#   direct messages  a direct message the full member reads, to a session that worked in the
#                    project alone, by GET /api/v1/messages/{id} and its /blocks, beside that
#                    session's list of messages, which the route index's row reads;
#   the agent view   the live socket's view of that session's conversation, which must answer
#                    the member as a session no one answers for;
#   the GitHub proxy every read packages/envoy/internal/dispatch/githubapi/proxy.go forwards
#                    (/api/github/rest/repos/{owner}/{repo}/...), filled with the repository
#                    only the project's issues resolve to, which must answer the member as a
#                    repository the App is not installed on answers, or refuse them 403.
# A GET row of a route that answers an object its caller may not see exactly as a missing one
# (`as_missing_routes`: whether the App is installed on a repository, which the server holds to
# the GitHub proxy's rule), filled with the project's objects, must answer the member exactly what
# it answers them for a missing object, status and text: no refusal of the project's own.
# Then the sockets:
#   /ws/doc/{room}  the member is not admitted to the room of the project's document, while the
#                   same probe admits the member to a document of their own project;
#   /ws/live        the member's event feed, open for the whole of SWEEP_LIVE_SECONDS, is sent no
#                   event of the project while the full member writes a comment on
#                   SWEEP_WRITE_ISSUE and a direct message that names no issue to the session the
#                   direct message above is to, which the full member's own feed, open over the
#                   same window, is sent; and the member's feed is sent the direct message the
#                   member writes in the window to their own session, SWEEP_MEMBER_SESSION, and
#                   reads it. A subscription the server ends or refuses on an open socket covers
#                   no window.
#
# "A row of the project" is a JSON member name or string value (`row_rule`) that is, whole, the
# project's key or a marker: an issue key of the project, an id the lookup found, the title of the
# issue the lookup filled with, or a session or a person of the project, one the lookup read as an
# author in the project that the full member finds in none of the projects the member sees, nor
# is the member. A string a person wrote (a `body`, `title`, `snippet` and the like) counts no
# project key and no issue key, even whole, and a found object only whole: a member's own comment
# may cite the project's items, its whole body a link, and the route index names example keys.
# Any other string with no whitespace, a value the server wrote, is one too when the key, an issue
# key or a marker stands in it as a whole part between separators: a reference, a path, a URL; a
# string with whitespace counts only whole. A body that is not JSON is read line by line by the
# same rule, and a socket's refused upgrade is read by the body the server wrote. Each answer is
# read for them once the values the path was filled with are masked, since an answer may echo
# what it was asked, and before it is compared with anything, so an answer alike for a missing
# object excuses no row. Filled paths are also called with objects that do not exist: any answer
# for the project's object (a 2xx, or a refusal other than 403 and 404) where a missing one is
# answered otherwise says the object exists, and is a row of it too, whatever the body holds.
# Every RFC 3339 instant and the durations a read measures are masked before two answers are
# compared. A session, a person, a repository, a priority, a broadcast and a delivery run belong
# to no project, so a row filled only with them is held to the markers alone. A line about the
# full member's feed names its frames' types and ids, never what they hold, and a line about
# either socket names the member's answer by its status or frame types, never a body the server
# wrote.
#
# Each row also says whether it can fail: a filled row can when the full member is answered the
# project's object otherwise than a missing one, a list when the full member's answer holds a row
# of the project. A row that cannot is called and judged all the same, and says so (`blind`): the
# full member is answered no row of the project there either. A full member's read a row needs
# that is answered no rule reads (401, 429, a 5xx, no answer), or an answer the sweep could not
# mask, leaves the run UNKNOWN, naming it: that row observed nothing. A GitHub proxy read or a row
# of a route held to a missing object's answer that cannot fail leaves the run UNKNOWN instead:
# its rule compares the member's answer with a missing object's, which the full member was
# answered alike too, so its answer then says nothing either way. A GitHub proxy read can fail
# only where the full member's read of the project's repository is answered, and a missing one's
# is the proxy's own answer for a repository the App is not installed on (503
# GITHUB_TOKEN_UNAVAILABLE); any other 5xx on either side, a rate limit or an outage, is no answer
# a rule reads.
#
# It runs in one of four modes, chosen by SWEEP_MODE and SWEEP_FILLS_FILE:
#   one run      SWEEP_MODE unset, no SWEEP_FILLS_FILE: both halves in one process, the full
#                member's (the lookup, the question and each row's can-fail call) and the
#                member's, with the live writes and their controls. The stack test runs this.
#   fills        SWEEP_MODE=fills, where a full member's credential may be held (a developer box):
#                the full member's half alone, written to SWEEP_FILLS_FILE. Exit 0 means the file
#                is whole; any other exit leaves no file. It makes the one write the lookup needs
#                (the question) and no comment.
#   runner       SWEEP_FILLS_FILE set, SWEEP_MODE unset, where no full member's credential may be
#                (a public runner): the member's half alone, each row's can-fail read from the
#                fills; a row the fills hold no line of was never asked of the full member, and is
#                UNKNOWN unless the member's answer FAILs it. It refuses a full member's
#                credential. The live check reads the member's feed over a window it prints
#                (`live window <W0> <W1>`, UTC), and writes the member's own direct message alone:
#                it holds only once a writer's delivered write falls strictly inside that window,
#                begun after W0 and ended before W1, which whoever ran both joins. W1 leaves the
#                writer's 15 s delivery bound before the feed closes and holds two writes at the
#                writer's 15 s pace, so SWEEP_LIVE_SECONDS is at least 45 on a runner. The agent
#                view's line says it could fail when the fills were made: whether the project's
#                session answers may have changed since.
#   writer       SWEEP_MODE=writer, beside a runner's run, with the fills: the full member's own
#                feed open, and every SWEEP_WRITE_EVERY s for SWEEP_WRITE_SECONDS one write, a
#                comment on SWEEP_WRITE_ISSUE and a direct message to the fills' session of the
#                project, and one line a write,
#                `write <began> <ended> <comment id> <message id> delivered` or `... not
#                delivered`, for whether that feed was sent both within SWEEP_WRITE_EVERY s, at
#                least 5 and at most the 15 a runner's window leaves: it began (UTC) before the
#                comment was asked for, and ended once the direct message was answered, so a
#                comment committed before the member's feed subscribed falls in no window. It sends
#                the project's session SWEEP_WRITE_SECONDS / SWEEP_WRITE_EVERY direct messages (40
#                by default), each a `btw` note a live session takes at its next turn; no route
#                deletes one. Exit 0 only when every write was.
#
# The fills file is ASCII, one `name<TAB>value` line a fill, its first line
# `restricted-sweep-fills<TAB>4`, then `project`, `origin_sha256` (the SHA-256 of the origin the
# fills were made at, a private one where they are made apart), `routes` (the SHA-256 of the route
# index they were made from), `deployment` (the SHA-256 of the key and creation time of each of
# the member's own projects, which tells one deployment from another and reads the same at its
# private origin and its public one) and `made_at` (Unix seconds). It holds no credential: the
# project's object ids and keys, its repository, the member's login and own projects, the found
# values that mark a row (ids, the project's own sessions and people, the reference prefix, and
# the target issue's title, which the search rows take as their text), one
# `named<TAB><kind> <value>` a found value that is a name a line prints by its kind alone (the
# project's repository, a person of it, a file of its conversation), one `can_fail<TAB><row>` a
# row whose full-member half can fail, and one `full_status<TAB><status> <row>` a row. Fills mode
# refuses to write one above 40 KiB, one that lacks a fill, or where a row it can tell cannot fail
# must (a GitHub proxy read, a route held to a missing object's answer). A runner refuses fills
# that are absent, empty, of another version or project, made from another route index than its
# own, older than 55 minutes (the question's run is kept an hour and read only by its asker),
# that lack a fill whole fills hold, that name their origin by no SHA-256, or that were made at
# another deployment, and names the origin they were made at, and the member's login, by the first
# 12 digits of their SHA-256 alone. Those digits keep the value itself out of the log; they do not
# hide a login or a host name someone can guess, whose SHA-256 anyone can take and compare. Fills
# mode and a runner each refuse a list of projects that gives no creation time of each of the
# member's own, which tells no deployment.
#
# Inputs, each refused by name when missing or malformed (the mode each is read in, in brackets):
#   SWEEP_ORIGIN             [all] the deployment's origin, https://<host>[:<port>]; http:// only
#                            to 127.0.0.1, localhost or [::1], for a local stack.
#   SWEEP_PROJECT            [all] the key of the project the member's access list must not name.
#   SWEEP_FULL_TOKEN         [one run, fills, writer] a bearer token of a full member who sees that
#                            project; any bearer Dispatch accepts, a broker-traded one included.
#   SWEEP_RESTRICTED_COOKIE  [one run, runner] the restricted member's session cookie, as the
#                            `Cookie` header carries it (`<name>=<value>`).
#   SWEEP_MEMBER_SESSION     [one run, runner] the member's own session, which the member's direct
#                            message of the live window is sent to, the control of their feed.
#   SWEEP_MEMBER             [fills] the restricted member's login. The member's own projects are
#                            the ones whose access lists name it, which a runner checks against
#                            the projects the member is listed.
#   SWEEP_FULL_COOKIE        [one run, fills, writer] the full member's session cookie, alike. The
#                            GitHub proxy and the live socket's agent view read as the browser's
#                            person and never as a token, and a direct message is a person's
#                            write, so only with it are the proxy reads and the agent view made as
#                            the full member too and able to fail, and the direct messages written;
#                            without it the proxy reads cannot fail, and the run is UNKNOWN, naming
#                            them, and fills mode writes no file.
#   SWEEP_WRITE_ISSUE        [one run, writer] the issue of SWEEP_PROJECT the full member comments
#                            on while the feeds are open. A comment, a direct message and the
#                            question are the full member's writes, and the member's own direct
#                            message the member's.
#   SWEEP_CONVERSATION_FILE  [one run, fills] `<session id>/<slug>`: a file of the conversation of
#                            a session that worked in SWEEP_PROJECT alone, which no route lists. The
#                            full member must read it, and the member must see the session
#                            nowhere. Left unset, or failing either, the two rows that read such a
#                            file are not filled, and the run is UNKNOWN.
#   SWEEP_FILLS_FILE         [fills, runner, writer] the fills file to write, or to read.
#   SWEEP_LIVE_SECONDS       [one run, runner] how long the feeds are read (default 60, maximum
#                            2147413, within the socket probe's timer range; at least 45 on a
#                            runner, whose window leaves the writer's 15 s delivery bound and holds
#                            two of its writes at 15 s).
#   SWEEP_WRITE_SECONDS, SWEEP_WRITE_EVERY
#                            [writer] how long the writer writes (default 600) and how often
#                            (default 15): whole seconds.
#   SWEEP_CALL_SECONDS       [all] how long one request waits (default 20).
#   SWEEP_PACE_SECONDS       [all] the pause before each request (default 0.1, so the run stays far
#                            under the edge's per-address request limit); 0 for none.
#   SWEEP_ROUTES, SWEEP_OPENAPI
#                            the route index and the API document to read, by default this
#                            checkout's.
# SWEEP_LIST_KINDS=1 prints, for each GET row of the route index, its path and the kind of object
# each wildcard is filled with (`{key}=issue`), and exits 4, never PASS's 0: its test holds that
# to the server's own route probes. SWEEP_LIST_PROXY=1 prints the GitHub proxy reads alike.
#
# It needs bash 3.2 or later, curl 7.55 or later, jq 1.6 or later, sha256sum or shasum (fills and
# runner modes), and bun with this checkout's dependencies installed (`bun install`), which runs
# the socket probe e2e/acceptance/socket-access.ts. No token or cookie is printed, written to the
# fills or passed on a command line: curl and the probe read them from files of mode 0600 in a
# directory of the run's own, which it removes. The files beside it hold its parts:
# restricted-sweep-lib.sh (requests and lines), -fills.sh (the fills file), -writer.sh (writer
# mode), -lookup.sh (the identities and the full member's lookup), -judge.sh (the row rule and the
# rows) and -sockets.sh (the sockets).
#
# It prints a line per call and a verdict:
#   PASS     exit 0: every GET row filled and called, every route and both sockets kept the
#            project from the member, every kind of object the project owns was found, at least
#            one row could fail, every full member's read a row needs was answered, every GitHub
#            proxy read and every row of a route held to a missing object's answer could fail, and
#            every live control held: the full member's own event feed was sent the comment and the
#            direct message (a runner: a writer's), and the member's feed their own direct
#            message, over the whole live window.
#   FAIL     exit 1: the member was answered a row of the project, admitted to its document's
#            room, sent an event of it or more of its session's conversation than that no one
#            answers for it, or a route held to a missing object's answer answered them otherwise
#            for the project's. The verdict names the first route that did. A FAIL line names the
#            route, the status and the row of the project the answer held (a key or an id; the
#            issue's title by its issue; the project's repository, a person of it or a file of its
#            conversation by that kind), never the answer's text: a public runner's log is public.
#   UNKNOWN  exit 2: anything that proved nothing, which is never a pass: an input refused, a
#            tool missing, an identity that is not what it must be, a kind of object the lookup
#            did not find, a row it could not fill, an answer no rule judges (401, 429, a 5xx, no
#            answer) to either member, no row that could fail, a GitHub proxy read or a row of a
#            route held to a missing object's answer that could not fail, fills a runner cannot
#            read, a subscription the server ended or refused, a socket control that did not
#            hold, or a GET route the deployment's own index (GET /api/v1) lists and routes.txt
#            lacks, which the sweep never called.
#   FILLS    exit 0 (fills mode): the file is whole, and where it is.
#   WRITER   exit 0 when every write was delivered, 1 when one was not (writer mode).
#
# Usage, from packages/dispatch:
#
#   SWEEP_ORIGIN=https://dispatch.example SWEEP_PROJECT=PRIV SWEEP_WRITE_ISSUE=PRIV-12 \
#     SWEEP_CONVERSATION_FILE=<session id>/<slug> SWEEP_FULL_TOKEN=... SWEEP_FULL_COOKIE=... \
#     SWEEP_RESTRICTED_COOKIE=... SWEEP_MEMBER_SESSION=<session id> bash e2e/acceptance/restricted-sweep.sh
#
#   # The split: fills and the writer where the full member's credential is, the runner apart.
#   SWEEP_MODE=fills SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_MEMBER=<login> SWEEP_FULL_TOKEN=... \
#     SWEEP_FULL_COOKIE=... SWEEP_CONVERSATION_FILE=<session id>/<slug> SWEEP_FILLS_FILE=fills.tsv \
#     bash e2e/acceptance/restricted-sweep.sh
#   SWEEP_MODE=writer SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_WRITE_ISSUE=PRIV-12 SWEEP_FULL_TOKEN=... \
#     SWEEP_FULL_COOKIE=... SWEEP_FILLS_FILE=fills.tsv bash e2e/acceptance/restricted-sweep.sh &
#   SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_FILLS_FILE=fills.tsv SWEEP_RESTRICTED_COOKIE=... \
#     SWEEP_MEMBER_SESSION=<session id> bash e2e/acceptance/restricted-sweep.sh
#
# Its tests: e2e/__tests__/restricted-sweep-judging.test.ts, -sockets.test.ts and -split.test.ts
# hold each judging rule alone against a fake Dispatch (restricted-sweep-fake.ts), the modes among
# them; e2e/__tests__/restricted-sweep.test.ts runs it, through restricted-sweep-stack.ts, against
# the local stack (e2e/harness-pool.ts, which starts e2e/run-server.sh) and against a stack of a
# build with no read check, which it must FAIL, each in one run and split into fills and a runner.

# A shell other than bash stops here, before it reads a line it would take another way.
if [ -z "${BASH_VERSION:-}" ]; then
  echo 'UNKNOWN: run this with bash: bash restricted-sweep.sh'
  exit 2
fi
# Credentials leave the inherited environment before any child is started, including tools used
# to find this checkout. Tracing must stop before their values are expanded.
set +x
set +a
full_token=${SWEEP_FULL_TOKEN:-}
cookie=${SWEEP_RESTRICTED_COOKIE:-}
full_cookie=${SWEEP_FULL_COOKIE:-}
unset SWEEP_FULL_TOKEN SWEEP_RESTRICTED_COOKIE SWEEP_FULL_COOKIE
export -n full_token cookie full_cookie
set -u
# Every pattern below matches bytes, whatever locale the machine runs in.
LC_ALL=C
export LC_ALL

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
routes=${SWEEP_ROUTES:-$root/packages/envoy/internal/dispatch/api/testdata/routes.txt}
openapi=${SWEEP_OPENAPI:-$root/packages/contracts/schemas/dispatch-openapi.json}
socket_probe=$here/socket-access.ts
work=''
watchers=''

# cleanup stops the socket probes still running and removes the run directory, credentials and
# all, however the run ends.
# shellcheck disable=SC2329 # the EXIT trap below runs it
cleanup() {
  local pid
  for pid in $watchers; do kill "$pid" 2>/dev/null; done
  for pid in $watchers; do wait "$pid" 2>/dev/null; done
  [ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# unknown MESSAGE: the verdict for a run that cannot go on.
unknown() {
  printf 'UNKNOWN: %s\n' "$1"
  exit 2
}

# shellcheck source-path=SCRIPTDIR source=restricted-sweep-lib.sh
. "$here/restricted-sweep-lib.sh"
# shellcheck source-path=SCRIPTDIR source=restricted-sweep-judge.sh
. "$here/restricted-sweep-judge.sh"
# shellcheck source-path=SCRIPTDIR source=restricted-sweep-fills.sh
. "$here/restricted-sweep-fills.sh"
# shellcheck source-path=SCRIPTDIR source=restricted-sweep-lookup.sh
. "$here/restricted-sweep-lookup.sh"
# shellcheck source-path=SCRIPTDIR source=restricted-sweep-writer.sh
. "$here/restricted-sweep-writer.sh"
# shellcheck source-path=SCRIPTDIR source=restricted-sweep-sockets.sh
. "$here/restricted-sweep-sockets.sh"

if [ "${SWEEP_LIST_PROXY:-}" = 1 ]; then
  printf '%s\n' "$proxy_reads"
  exit 4
fi
if [ "${SWEEP_LIST_KINDS:-}" = 1 ]; then
  [ -r "$routes" ] || unknown "the route index $routes cannot be read"
  while read -r method pattern _ || [ -n "${method:-}" ]; do
    [ "$method" = GET ] || continue
    line=$pattern
    for wildcard in $(wildcards_of "$pattern"); do
      line="$line $wildcard=$(kind_of "$pattern" "$wildcard")"
    done
    printf '%s\n' "$line"
  done <"$routes"
  exit 4
fi

origin=${SWEEP_ORIGIN:-}
origin=${origin%/}
case $origin in
  https://*) authority=${origin#https://} ;;
  http://127.0.0.1 | http://127.0.0.1:* | http://localhost | http://localhost:* | 'http://[::1]' | 'http://[::1]:'*)
    authority=${origin#http://} ;;
  '') unknown 'SWEEP_ORIGIN is required: the deployment origin, https://<host>[:<port>]' ;;
  *) unknown "SWEEP_ORIGIN must be https://<host>[:<port>], or http:// to 127.0.0.1, localhost or [::1]: $origin" ;;
esac
# A host and a port alone: no path, query, user or space.
case $authority in
  '' | *[!]A-Za-z0-9.:[-]*) unknown "SWEEP_ORIGIN must be an origin alone, https://<host>[:<port>]: $origin" ;;
esac
# A project key as Dispatch's own rule has it: a capital letter, then one to nine capitals or digits.
project=${SWEEP_PROJECT:-}
[ -n "$project" ] || unknown 'SWEEP_PROJECT is required: the key of the project the member is not listed on'
case $project in
  [A-Z]?*) case ${project#?} in *[!A-Z0-9]*) unknown "SWEEP_PROJECT is no project key: $project" ;; esac ;;
  *) unknown "SWEEP_PROJECT is no project key: $project" ;;
esac
[ "${#project}" -le 10 ] || unknown "SWEEP_PROJECT is no project key: $project"
# The mode: fills and writer by name; a fills file read by a runner; else one run.
mode=${SWEEP_MODE:-}
fills_file=${SWEEP_FILLS_FILE:-}
case $mode in
  fills | writer) ;;
  '') if [ -n "$fills_file" ]; then mode=runner; else mode=one; fi ;;
  *) unknown "SWEEP_MODE must be fills or writer, or unset: $mode" ;;
esac
case $mode in
  fills)
    [ -n "$fills_file" ] || unknown 'SWEEP_FILLS_FILE is required in fills mode: the file to write the fills to'
    case $fills_file in */) unknown "SWEEP_FILLS_FILE must name a file: $fills_file" ;; esac
    ;;
  writer)
    [ -n "$fills_file" ] ||
      unknown "SWEEP_FILLS_FILE is required in writer mode: the fills a run of SWEEP_MODE=fills wrote, whose session of $project the writer's direct messages go to"
    ;;
esac
# The full member's credential: required where the full member's half runs, and refused by name on
# a runner, whose fills already hold that half.
if [ "$mode" = runner ]; then
  [ -z "$full_token" ] && [ -z "$full_cookie" ] ||
    unknown 'a runner holds no full member'"'"'s credential: unset SWEEP_FULL_TOKEN and SWEEP_FULL_COOKIE, whose half SWEEP_FILLS_FILE holds'
else
  [ -n "$full_token" ] ||
    unknown "SWEEP_FULL_TOKEN is required: a full member's bearer token that sees $project, or, where no full member's credential may be, SWEEP_FILLS_FILE: the fills a run of SWEEP_MODE=fills wrote"
  case $full_token in *[!A-Za-z0-9._~+/=-]*) unknown 'SWEEP_FULL_TOKEN holds a byte no bearer token holds' ;; esac
fi
member_session=''
if [ "$mode" = one ] || [ "$mode" = runner ]; then
  [ -n "$cookie" ] || unknown 'SWEEP_RESTRICTED_COOKIE is required: the restricted member'"'"'s session cookie, <name>=<value>'
  holds_line_break "$cookie" && unknown 'SWEEP_RESTRICTED_COOKIE holds a line break'
  case $cookie in *=*) ;; *) unknown 'SWEEP_RESTRICTED_COOKIE must be <name>=<value>, as the Cookie header carries it' ;; esac
  case $cookie in *[!\ -~]*) unknown 'SWEEP_RESTRICTED_COOKIE holds a non-printable header byte' ;; esac
  member_session=${SWEEP_MEMBER_SESSION:-}
  case $member_session in
    '') unknown "SWEEP_MEMBER_SESSION is required: the member's own session, which the member's direct message of the live window is sent to, the control of their feed" ;;
    *[!A-Za-z0-9._-]*) unknown "SWEEP_MEMBER_SESSION is no session id: $member_session" ;;
  esac
fi
if [ -n "$full_cookie" ]; then
  holds_line_break "$full_cookie" && unknown 'SWEEP_FULL_COOKIE holds a line break'
  case $full_cookie in *=*) ;; *) unknown 'SWEEP_FULL_COOKIE must be <name>=<value>, as the Cookie header carries it' ;; esac
  case $full_cookie in *[!\ -~]*) unknown 'SWEEP_FULL_COOKIE holds a non-printable header byte' ;; esac
fi
# The member's login, which fills mode reads the member's own projects by.
sweep_member=''
if [ "$mode" = fills ]; then
  sweep_member=${SWEEP_MEMBER:-}
  case $sweep_member in
    '') unknown "SWEEP_MEMBER is required in fills mode: the restricted member's login" ;;
    *[!\!-~]* | *@*@* | @* | *@) unknown "SWEEP_MEMBER is no login: $sweep_member" ;;
    ?*@?*) ;;
    *) unknown "SWEEP_MEMBER is no login: $sweep_member" ;;
  esac
fi
write_issue=''
if [ "$mode" = one ] || [ "$mode" = writer ]; then
  write_issue=${SWEEP_WRITE_ISSUE:-}
  case $write_issue in
    '') unknown "SWEEP_WRITE_ISSUE is required: the issue of $project the full member comments on" ;;
    "$project"-[1-9]*) is_count "${write_issue#"$project"-}" || unknown "SWEEP_WRITE_ISSUE is no issue of $project: $write_issue" ;;
    *) unknown "SWEEP_WRITE_ISSUE is no issue of $project: $write_issue" ;;
  esac
fi
# Every fill of fill_names, which the lookup sets or a runner and a writer read, starts empty.
for fill_name in $fill_names; do printf -v "$fill_name" '%s' ''; done
# A file of a conversation: the session id, a "/", and the file's slug, neither holding a "/".
conversation_file=''
conversation_why='SWEEP_CONVERSATION_FILE names no file of a conversation, and no route lists them'
if [ "$mode" = one ] || [ "$mode" = fills ]; then
  conversation_file=${SWEEP_CONVERSATION_FILE:-}
  case $conversation_file in
    '') ;;
    */*/* | /* | */ | *[!A-Za-z0-9._/-]*)
      unknown "SWEEP_CONVERSATION_FILE must be <session id>/<slug>: $conversation_file" ;;
    */*) conversation_session=${conversation_file%/*} conversation_slug=${conversation_file#*/} ;;
    *) unknown "SWEEP_CONVERSATION_FILE must be <session id>/<slug>: $conversation_file" ;;
  esac
fi
live_seconds=${SWEEP_LIVE_SECONDS:-60}
is_count "$live_seconds" && [ "$live_seconds" -gt 0 ] && [ "$live_seconds" -le 2147413 ] ||
  unknown "SWEEP_LIVE_SECONDS must be a whole number of seconds, at least 1 and at most 2147413: $live_seconds"
# The probe accepts canonical decimal seconds, and its timers must fit a signed 32-bit millisecond delay.
while [ "${live_seconds#0}" != "$live_seconds" ]; do live_seconds=${live_seconds#0}; done
# A runner's window leaves the writer's delivery bound before its feed closes, and must hold two
# writes at the writer's pace for one to fall inside it (runner_live_floor).
[ "$mode" != runner ] || [ "$live_seconds" -ge "$runner_live_floor" ] ||
  unknown "SWEEP_LIVE_SECONDS must be at least $runner_live_floor on a runner, whose live window leaves the writer's $delivery_seconds s delivery bound before its feed closes and holds two of its writes at 15 s: $live_seconds"
write_seconds=${SWEEP_WRITE_SECONDS:-600}
write_every=${SWEEP_WRITE_EVERY:-15}
if [ "$mode" = writer ]; then
  is_count "$write_seconds" && [ "$write_seconds" -gt 0 ] && [ "$write_seconds" -le 2147000 ] ||
    unknown "SWEEP_WRITE_SECONDS must be a whole number of seconds, at least 1: $write_seconds"
  is_count "$write_every" && [ "$write_every" -gt 0 ] ||
    unknown "SWEEP_WRITE_EVERY must be a whole number of seconds, at least 1: $write_every"
  while [ "${write_seconds#0}" != "$write_seconds" ]; do write_seconds=${write_seconds#0}; done
  while [ "${write_every#0}" != "$write_every" ]; do write_every=${write_every#0}; done
fi
call_seconds=${SWEEP_CALL_SECONDS:-20}
is_count "$call_seconds" && [ "$call_seconds" -gt 0 ] || unknown "SWEEP_CALL_SECONDS must be a whole number of seconds, at least 1: $call_seconds"
pace=${SWEEP_PACE_SECONDS:-0.1}
is_seconds "$pace" || unknown "SWEEP_PACE_SECONDS must be a number of seconds: $pace"

for tool in curl jq bun mktemp sed awk grep sort comm; do
  command -v "$tool" >/dev/null 2>&1 || unknown "$tool is not on PATH"
done
curl_version=$(curl --version 2>/dev/null | awk 'NR == 1 { print $2 }')
curl_major=${curl_version%%.*}
curl_minor=${curl_version#*.}
curl_minor=${curl_minor%%.*}
if ! is_count "$curl_major" || ! is_count "$curl_minor"; then
  unknown "curl reports no version this script reads: $curl_version"
fi
if [ "$curl_major" -lt 7 ] || { [ "$curl_major" -eq 7 ] && [ "$curl_minor" -lt 55 ]; }; then
  unknown "curl $curl_version is older than 7.55, which reads a header from a file"
fi
[ -r "$routes" ] || unknown "the route index $routes cannot be read"
[ -r "$openapi" ] || unknown "the API document $openapi cannot be read"
[ -r "$socket_probe" ] || unknown "the socket probe $socket_probe cannot be read"
# The route index's SHA-256, which fills name and a runner holds them to: fills made from another
# judged other rows.
routes_sha256=''
if [ "$mode" = fills ] || [ "$mode" = runner ]; then
  routes_sha256=$(sha256_of <"$routes")
  [ -n "$routes_sha256" ] || unknown 'neither sha256sum nor shasum is on PATH, which the fills name the route index by'
fi

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/restricted-sweep.XXXXXXXX") || unknown 'mktemp could not make the run directory'
if [ "$mode" != runner ]; then
  printf 'Authorization: Bearer %s\n' "$full_token" >"$work/full.h" || unknown 'could not write the full member header'
fi
if [ "$mode" = one ] || [ "$mode" = runner ]; then
  printf 'Cookie: %s\n' "$cookie" >"$work/restricted.h" || unknown 'could not write the restricted member header'
fi
# The full member as the browser's person, for the GitHub proxy, the agent view and a direct
# message, which read no token.
full_browser=full
if [ -n "$full_cookie" ]; then
  printf 'Cookie: %s\n' "$full_cookie" >"$work/full-browser.h" || unknown 'could not write the full member cookie header'
  full_browser=full-browser
fi
unset full_token cookie full_cookie
for record in failed unknown values named can-fail full-status missing own; do
  : >"$work/$record" || unknown "could not make the run's $record record"
done
# Fills mode leaves no file but a whole one: an older file at the path goes before the first request.
if [ "$mode" = fills ]; then
  [ ! -d "$fills_file" ] || unknown "SWEEP_FILLS_FILE names a directory: $fills_file"
  rm -f -- "$fills_file" || unknown "could not remove the older SWEEP_FILLS_FILE: $fills_file"
fi
going_why=''
member=''
# What a runner and a writer read of the fills, every fill before the first request.
if [ "$mode" = runner ] || [ "$mode" = writer ]; then read_fills; fi

printf 'restricted-sweep: %s, project %s, %s mode, %s\n' "$origin" "$project" "$mode" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
[ "$mode" != writer ] || run_writer

identities
if [ "$mode" = runner ]; then runner_projects; else lookup; fi
write_markers
started=$SECONDS
sweep_routes

# Fills mode ends here, with the full member's half of every row: the fills, whole, in
# SWEEP_FILLS_FILE, or no file and the reasons. The sockets are the member's alone, a runner's.
if [ "$mode" = fills ]; then
  printf 'swept   %s GET rows: %s rows, %s can fail, %s blind, %s not filled; %s requests in %s s\n' \
    "$rows" "$judged" "$live" "$blind" "$unfilled" "$calls" "$((SECONDS - started))"
  [ "$live" -gt 0 ] || printf 'no row could fail: the full member is answered no row of %s anywhere\n' "$project" >>"$work/unknown"
  [ "$judged" -gt 0 ] || printf 'no route was judged\n' >>"$work/unknown"
  if [ -s "$work/missing" ] || [ -s "$work/unknown" ]; then
    sed 's/^/  /' "$work/missing" "$work/unknown"
    printf 'UNKNOWN: no fills were written: the full member'"'"'s half of this run did not prove the %s reasons above\n' \
      "$(cat "$work/missing" "$work/unknown" | wc -l | tr -d ' ')"
    exit 2
  fi
  write_fills
  exit 0
fi

doc_socket
live_socket

printf 'swept   %s GET rows: %s calls judged, %s can fail, %s blind, %s not filled; %s requests in %s s\n' \
  "$rows" "$judged" "$live" "$blind" "$unfilled" "$calls" "$((SECONDS - started))"
if [ -s "$work/failed" ]; then
  printf 'FAIL: %s answers gave the member what %s holds, the first: %s\n' "$(wc -l <"$work/failed" | tr -d ' ')" "$project" "$(head -n 1 "$work/failed")"
  exit 1
fi
[ "$live" -gt 0 ] || printf 'no row could fail: the full member is answered no row of %s anywhere\n' "$project" >>"$work/unknown"
[ "$judged" -gt 0 ] || printf 'no route was judged\n' >>"$work/unknown"
if [ -s "$work/missing" ] || [ -s "$work/unknown" ]; then
  # The verdict is the last line, as a reader of the log's end takes it.
  sed 's/^/  /' "$work/missing" "$work/unknown"
  printf 'UNKNOWN: nothing reached the member, but this run did not prove it, for the %s reasons above\n' \
    "$(cat "$work/missing" "$work/unknown" | wc -l | tr -d ' ')"
  exit 2
fi
verdict="PASS: no route and neither socket gave the member a row of $project"
if [ "$mode" = runner ] && [ -n "$window_line" ]; then
  verdict="$verdict; the live check holds once a writer's delivered write falls inside the live window $window_line, which the tailnet leg joins"
fi
printf '%s\n' "$verdict"
exit 0
