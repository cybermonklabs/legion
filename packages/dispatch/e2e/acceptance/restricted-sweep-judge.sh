# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # restricted-sweep.sh sources this file: it sets what this reads, and reads what this sets
# The restricted sweep's rows (restricted-sweep.sh sources this file, beside it): the rule that
# says whether an answer holds a row of the project, the masking two answers are compared under,
# and each kind of row in its two halves, the full member's and the member's, which one driver
# (`row`) runs as the run's mode has it. It defines functions, its constants and the tallies a
# run starts at zero; they read the entry script's fills, `mode`, `project`, `routes`, `openapi` and
# the run's files.

# wildcards_of PATTERN prints the name of each {wildcard} of PATTERN, one per line.
wildcards_of() {
  local rest=$1
  while :; do
    case $rest in
      *'{'*'}'*)
        rest=${rest#*\{}
        printf '%s\n' "${rest%%\}*}"
        rest=${rest#*\}}
        ;;
      *) return 0 ;;
    esac
  done
}

# kind_of PATTERN WILDCARD prints the kind of object WILDCARD names in PATTERN, as the server's
# route probes name it (internal/dispatch/api/cmd/routeprobes), or nothing for a wildcard this
# sweep does not know.
kind_of() {
  case $2 in
    key)
      case $1 in
        /api/v1/issues/*) echo issue ;;
        /api/v1/projects/* | /api/v1/access/projects/*) echo project ;;
      esac
      ;;
    id)
      case $1 in
        */artifacts/\{id\}* | /api/v1/me/documents/\{id\}*) echo document ;;
        */asks/\{id\}*) echo ask ;;
        */comments/\{id\}*) echo comment ;;
        */messages/\{id\}*) echo message ;;
        */handoffs/\{id\}*) echo handoff ;;
        */broadcasts/\{id\}*) echo broadcast ;;
        */priorities/\{id\}*) echo priority ;;
        */near-copy-checks/\{id\}*) echo near-copy-check ;;
        /api/v1/going/requests/\{id\}*) echo going-run ;;
        /api/v1/delivery/runs/\{id\}*) echo delivery-run ;;
      esac
      ;;
    block_id) echo block ;;
    number) echo document-version ;;
    slug) echo document-slug ;;
    phase) echo handoff-phase ;;
    session_id) echo session ;;
    login) echo person ;;
    owner) echo repository-owner ;;
    repo) echo repository-name ;;
  esac
}

# The GitHub proxy's reads, after /api/github/rest/repos/{owner}/{repo}/, one a line: every path
# packages/envoy/internal/dispatch/githubapi/proxy.go's `readable` forwards, the only GET the
# proxy answers, which routes.txt does not list. Its test holds this list to that expression.
readonly proxy_reads='pulls/{number}
issues/{number}
commits/{sha}/check-runs
actions/runs?head_sha={sha}'
# What the proxy answers a repository the App is not installed on, a hidden one included (503
# with this code, githubapi/proxy.go's `unavailable`), which its test holds to that function. Any
# other 5xx - a rate limit, which the proxy answers every repository alike, or an outage - is no
# answer a rule reads.
readonly not_installed_code=GITHUB_TOKEN_UNAVAILABLE

# The GET rows whose wildcards the sweep fills that answer an object the caller may not see exactly
# as a missing one, one a line: whether the App is installed on a repository, which the server
# holds to the GitHub proxy's rule (packages/envoy/internal/dispatch/api/github_app_installation.go,
# refs.RepositoryClause), so a repository only the project's issues resolve to answers the member
# as one the App is not installed on, status and bytes.
readonly as_missing_routes='/api/v1/github/app-installation/{owner}/{repo}'

# answers_as_missing PATTERN: whether the GET row PATTERN is one of as_missing_routes.
answers_as_missing() {
  case $'\n'"$as_missing_routes"$'\n' in *$'\n'"$1"$'\n'*) return 0 ;; esac
  return 1
}

# write_markers: the markers file marker_in reads: the project's key, the found values (`values`,
# one a line) by value, each found value that holds a separator, with its regex spelling, for the
# rule that reads a value inside a reference, and the member names whose strings a person writes.
write_markers() {
  jq -Rn --arg key "$project" '
    [inputs | select(length > 0)] as $found
    | {key: $key,
       issue: ("^" + $key + "-[1-9][0-9]*$"),
       separator: "[/:#?&=]",
       values: (($found | map({(.): true}) | add) // {}),
       multi: [$found[] | select(test("[/:#?&=]") and (test("\\s") | not))
         | {value: ., re: gsub("(?<c>[.*+?^${}()|\\[\\]\\\\])"; "\\\(.c)")}],
       text: (["body", "title", "snippet", "text", "markdown", "content", "question", "description",
         "words", "summary", "name"] | map({(.): true}) | add)}' \
    "$work/values" >"$work/markers.json" || unknown 'jq could not write the markers'
}

# The one rule that says whether a string an answer holds is a row of the project, as a jq
# function of the string, the markers file and whether a person wrote the string. A string is
# one when it is, whole, the project's key or a value the lookup found (an issue key, an id, the
# issue's title, a session of the project alone). A string a person wrote - the value of a member
# such as `body`, `title` or `snippet` (`text` in the markers file) - counts no project key and no
# issue key, even whole: a member's own comment may cite the project's items by any form, its
# whole body or its first line one key, and the route index's own text names example keys of any
# project; it counts only a found value, whole. Any other string with no whitespace in it is a
# value the server wrote - a key, an id, a reference, a path or a URL - and it is one too when the
# key, an issue key of the project or a found value stands in it as a whole part between
# separators (`/ : # ? & =`) or its ends: `KEY-12/spec`, a reference prefix and `KEY-12`,
# `/issues/KEY-12`, `/projects/KEY`, a dashboard URL, `session:<id>`. A member this rule does not
# name as a person's is read as the server's, so a new text member fails closed.
# shellcheck disable=SC2016 # the `$` names in it are jq's variables, never the shell's
readonly row_rule='
  def row($m; $written):
    . as $s
    | if $written and ($s == $m.key or ($s | test($m.issue))) then empty
      elif $s == $m.key or $m.values[$s] != null then $s
      elif $written or ($s | test("\\s")) then empty
      else first(
          ($s | splits($m.separator)
            | select(. == $m.key or $m.values[.] != null
                or (startswith($m.key + "-") and test($m.issue)))),
          ($m.multi[] | . as $v
            | select($s | test("(^|" + $m.separator + ")" + $v.re + "(" + $m.separator + "|$)"))
            | $v.value))
      end;'

# marker_in FILE prints the first string FILE holds that `row_rule` names a row of the project.
# A JSON body (a stream is read in full; false and null are JSON too) is read as each string value,
# with the member name it stands under, and each member name. A body jq cannot read as JSON is read
# as each of its lines and each double-quoted string in them, as the server's, by the same rule. An
# answer no jq reads is recorded as unproven, never as clean. One jq a body: the sweep reads
# thousands.
marker_in() {
  jq -r -s --slurpfile m "$work/markers.json" "$row_rule"'
    $m[0] as $m
    | first(
        (paths(type == "string") as $p
          | ([$p[] | strings] | last // "") as $member
          | getpath($p) | row($m; $m.text[$member] != null)),
        (.. | objects | keys[] | row($m; false))) // empty' "$1" 2>/dev/null &&
    return
  jq -r -R -s --slurpfile m "$work/markers.json" "$row_rule"'
    $m[0] as $m
    | first(splits("\n") | select(length > 0)
        | (., (split("\"") as $parts | $parts | keys[] | select(. % 2 == 1) | $parts[.]))
        | row($m; false)) // empty' "$1" 2>/dev/null ||
    printf 'an answer the markers could not be read in\n' >>"$work/unknown"
}

# shown MARKER: a row of the project as a line names it: a key or an id the lookup found, never
# text a person wrote, so the issue's title is named by its issue, and never a name: a value the
# run's `named` gives a kind (the project's repository, a person of the project alone, a file of
# its conversation) is named by that kind. A line names what the answer held, never the answer:
# the log of a run on a public runner is public.
shown() {
  if [ -n "$issue_title" ] && [ "$1" = "$issue_title" ]; then
    printf 'the title of %s' "$issue"
    return
  fi
  case $(word_before "$work/named" "$1") in
    repository) printf "the project's repository" ;;
    person) printf 'a person of the project' ;;
    conversation) printf "the project's conversation" ;;
    *) printf '%s' "$1" ;;
  esac
}

# masked FILE VALUE... masks literal fills, their URL and JSON encodings, every RFC 3339 instant
# and the durations a read measures, so two answers read at two moments for two fills compare
# alike when they say the same: a read's own `as_of`, `computed_at` and window bounds differ at
# every read, and a row's own times come with its ids, which the markers read. A masked JSON
# answer stays JSON, the duration's mask a string, so the markers read it as JSON.
masked() {
  local file=$1
  shift
  jq -nr --rawfile body "$file" --arg project "$project" '
    reduce $ARGS.positional[] as $value ($body;
      if $value == "" then .
      elif $value == $project then
        gsub("(?<![A-Za-z0-9-])" + $value + "(?![A-Za-z0-9-])"; "<fill>")
      else
        reduce ([$value, ($value | @uri), ($value | tojson | .[1:-1])] | unique[]) as $literal
          (.; split($literal) | join("<fill>"))
      end)
    | gsub("[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})";
        "<time>")
    | gsub("\"(?<key>took_ms|age_seconds|oldest_lacking_seconds)\"[[:space:]]*:[[:space:]]*[0-9.]+";
        "\"\(.key)\":\"<n>\"")' --args "$@"
}

# masked_marker FILE VALUE...: sets `marker` to the first row of the project FILE holds once each
# VALUE, a value the request was filled with and which the answer may echo, is masked, or to
# nothing; it returns 1, `marker` empty, where the answer could not be masked, so it was not read
# for rows at all, which its caller judges. It sets a variable rather than printing one: inside
# $(...) a failure could stop nothing but that subshell, and its words would read as the marker.
masked_marker() {
  local file=$1
  shift
  marker=''
  masked "$file" "$@" >"$work/masked" || return 1
  marker=$(marker_in "$work/masked")
}

# alike STATUS_A FILE_A STATUS_B FILE_B VALUE...: whether two answers say the same, one status and
# one text, once each VALUE is masked in both.
alike() {
  local a=$1 file_a=$2 b=$3 file_b=$4 text_a text_b
  shift 4
  [ "$a" = "$b" ] || return 1
  text_a=$(masked "$file_a" "$@") || unknown 'could not mask an answer'
  text_b=$(masked "$file_b" "$@") || unknown 'could not mask an answer'
  [ "$text_a" = "$text_b" ]
}

# lines_into NAME TEXT: the array NAME, of TEXT's lines that are not empty.
lines_into() { IFS=$'\n' read -r -d '' -a "$1" <<<"$2" || :; }

live=0 blind=0 judged=0 unfilled=0 in_fills=1 must_fail=0 can_text=''
# counted CAN: counts the row as one that can fail (CAN 1) or a blind one, and sets can_text to
# which, for the row's line.
counted() {
  judged=$((judged + 1))
  if [ "$1" = 1 ]; then
    live=$((live + 1))
    can_text='can fail'
  else
    blind=$((blind + 1))
    can_text=blind
  fi
}

# unfailable: whether the row in hand is of a kind whose rule compares the member's answer with a
# missing object's (as_missing, proxy: `must_fail`) and cannot fail: the full member was answered
# alike for both, so the member's answer then says nothing either way, and the row is UNKNOWN.
unfailable() { [ "$must_fail" = 1 ] && [ "$can" = 0 ]; }

# held LABEL TEXT: the row LABEL kept the project from the member, as TEXT says, and whether it
# can fail. A runner's row its fills hold no line of (`in_fills` 0), or a row that is unfailable,
# proved nothing by it, so it is UNKNOWN for the row's `why` in place of an ok line.
held() {
  if [ "$in_fills" = 0 ]; then
    unproven "$1: $why"
  elif unfailable; then
    unproven "$1: the member is answered $2, but $why"
  else
    printf 'ok      %s: %s (%s)\n' "$1" "$2" "$can_text"
  fi
}

# member_marker LABEL FILE VALUE...: masked_marker over the member's answer FILE, the row LABEL's;
# where the answer could not be masked, it was not read for rows, and the row is UNKNOWN, naming
# it: it returns 1.
member_marker() {
  local label=$1
  shift
  masked_marker "$@" && return 0
  unproven "$label: the member's answer could not be masked, so it was not read for rows of $project"
  return 1
}

# row KIND LABEL ARGS...: the row LABEL, of KIND, with ARGS. Its full member's half,
# `KIND_full LABEL ARGS...`, sets can (1 where the row can fail), ft (the full member's status of
# it) and why (why it cannot fail), and control_failed where a read of its own was answered no
# rule reads; a runner reads can and ft from the fills instead. A control that failed leaves the
# run UNKNOWN, naming the read, and the member's half is not run: the row observed nothing, so it
# is counted neither as a row that can fail nor as a blind one. Fills mode keeps the row's half
# (put_row) or, where the row is unfailable, leaves the run UNKNOWN (no file). Every other mode
# then runs the member's half, `KIND_member LABEL ARGS...`, whose `held` makes an unfailable row
# UNKNOWN too. A runner's row its fills hold no line of was never asked of the full member (the
# fills were made from another API document; another route index read_fills refuses): its
# member's half runs all the same, so a row of the project in the answer FAILs, but nothing else
# the answer says proves the row, which `held` leaves UNKNOWN, never blind.
row() {
  local kind=$1 label=$2
  shift 2
  must_fail=0
  case $kind in as_missing | proxy) must_fail=1 ;; esac
  can=0 ft=- why='' control_failed='' in_fills=1 can_text=''
  if [ "$mode" = runner ]; then
    can=$(fills_can "$label")
    ft=$(fills_status "$label")
    why="SWEEP_FILLS_FILE holds no full member's read of it that could fail"
    if [ -z "$ft" ]; then
      in_fills=0 can=0 ft=-
      why="SWEEP_FILLS_FILE holds no full member's read of it: the fills were made from another API document"
      "${kind}_member" "$label" "$@"
      return
    fi
  else
    "${kind}_full" "$label" "$@"
  fi
  if [ -n "$control_failed" ]; then
    unproven "$label: $why"
    return
  fi
  counted "$can"
  if [ "$mode" = fills ]; then
    if unfailable; then
      unproven "$label: $why"
      return
    fi
    put_row "$label"
    printf 'fill    %s (%s)\n' "$label" "$can_text"
    return
  fi
  "${kind}_member" "$label" "$@"
}

# The kinds of row, each a KIND_full and a KIND_member function of the row's label and arguments.
#
# filled TARGET MISSING STRICT TARGET_FILLS MISSING_FILLS: a row whose path names objects, called
# as TARGET (filled with the project's objects) and, where an answer needs it, as MISSING (filled
# with objects that do not exist). The fills are newline-separated lists of the values each side
# was filled with. An answer is read for rows with its own side's fills masked; two answers are
# compared with both sides' fills masked in each, since an answer may hold a fill's text that is
# no echo of it - a member's own comment that names the project's key - and that text must read
# alike on both sides. STRICT is 1 when an object of the project fills the row, which then must
# answer the member as a missing one, or refuse. It can fail where the full member is answered
# the project's object otherwise than a missing one, or a row of it beyond what the request echoes.
filled_full() {
  local target=$2 missing=$3 strict=$4 fm='' marker
  local -a t_list both_list
  lines_into t_list "$5"
  lines_into both_list "$5"$'\n'"$6"
  call full "$target" "$work/ft"
  ft=$status
  if ! is_judgeable "$ft"; then
    control_failed=1 why="the full member is answered $ft, so its control was not read: $(head_of "$work/ft")"
    return
  fi
  if is_2xx "$ft" && [ "$strict" = 1 ]; then
    call full "$missing" "$work/fm"
    fm=$status
    if is_judgeable "$fm" && ! alike "$ft" "$work/ft" "$fm" "$work/fm" ${both_list[@]+"${both_list[@]}"}; then
      can=1
    fi
  fi
  if [ "$can" = 0 ] && ! is_refusal "$ft"; then
    if ! masked_marker "$work/ft" ${t_list[@]+"${t_list[@]}"}; then
      control_failed=1 why="the full member's answer could not be masked, so it was not read for rows of $project"
      return
    fi
    [ -z "$marker" ] || can=1
  fi
  if [ "$can" = 0 ] && [ -n "$fm" ] && ! is_judgeable "$fm"; then
    control_failed=1 why="the full member is answered $fm for a missing one, so its control was not read: $(head_of "$work/fm")"
  fi
}
filled_member() {
  local label=$1 target=$2 missing=$3 strict=$4 rt rm marker
  local -a t_list both_list
  lines_into t_list "$5"
  lines_into both_list "$5"$'\n'"$6"
  call restricted "$target" "$work/rt"
  rt=$status
  if ! is_judgeable "$rt"; then
    unproven "$label: the member is answered $rt, which no rule judges"
    return
  fi
  # The answer is read for rows before it is compared with anything, so an answer alike for a
  # missing object, a filter the server ignored, excuses none.
  member_marker "$label" "$work/rt" ${t_list[@]+"${t_list[@]}"} || return 0
  if [ -n "$marker" ]; then
    if is_refusal "$rt"; then
      fail "$label: the refusal $rt holds $(shown "$marker"), a row of $project"
    else
      fail "$label: the member is answered $rt holding $(shown "$marker"), a row of $project"
    fi
    return
  fi
  if is_refusal "$rt"; then
    held "$label" "$rt"
    return
  fi
  call restricted "$missing" "$work/rm"
  rm=$status
  if alike "$rt" "$work/rt" "$rm" "$work/rm" ${both_list[@]+"${both_list[@]}"}; then
    held "$label" "$rt, as a missing one is answered"
  elif is_2xx "$rt" && [ "$strict" = 0 ]; then
    held "$label" "$rt, no row of $project"
  elif is_2xx "$rt" && is_refusal "$rm"; then
    # The object exists for the member where a missing one does not, an empty answer included.
    fail "$label: the member is answered $rt for $project's object, where a missing one is refused $rm"
  elif [ "$strict" = 1 ] && is_judgeable "$rm"; then
    # Any answer of its own for the project's object, a refusal such as 400 ARTIFACT_LINKED
    # included, where a missing one is answered otherwise, says the object exists.
    fail "$label: the member is answered $rt for $project's object, otherwise than a missing one answered $rm"
  else
    unproven "$label: the member is answered $rt, where a missing one is answered $rm"
  fi
}

# list PATH: a row whose path names no object, called bare. Its answer to the member may hold the
# member's own rows, never one of the project; a refusal of any other 4xx is judged alike for both
# callers. It can fail where the full member's answer holds a row of the project.
list_full() {
  call full "$2" "$work/ft"
  ft=$status
  if ! is_judgeable "$ft"; then
    control_failed=1 why="the full member is answered $ft, so its control was not read: $(head_of "$work/ft")"
    return
  fi
  if is_2xx "$ft" && [ -n "$(marker_in "$work/ft")" ]; then can=1; fi
}
list_member() {
  local label=$1 rt marker
  call restricted "$2" "$work/rt"
  rt=$status
  marker=$(marker_in "$work/rt")
  if is_judgeable "$rt" && [ -n "$marker" ]; then
    fail "$label: the member is answered $rt holding $(shown "$marker"), a row of $project"
    return
  fi
  if is_refusal "$rt"; then
    held "$label" "$rt"
  elif is_2xx "$rt"; then
    held "$label" "$rt, no row of $project"
  elif is_judgeable "$rt" && [ "$rt" = "$ft" ]; then
    held "$label" "$rt, as the full member is answered"
  else
    unproven "$label: the member is answered $rt, the full member $ft"
  fi
}

# as_missing TARGET MISSING TARGET_FILLS MISSING_FILLS: a row of a route that answers an object its
# caller may not see exactly as a missing one (`as_missing_routes`), called as TARGET and as
# MISSING. The member must be answered for TARGET exactly what they are for MISSING, status and
# text with both sides' fills masked: any other answer, a refusal of TARGET alone among them,
# tells them the project's object is there. It can fail only where the full member is answered
# TARGET otherwise than MISSING (the App installed on the project's repository, and on no missing
# one); a server that answered every caller as for a missing object would otherwise pass too. The
# answer is read for rows of the project first, as every row's is.
as_missing_full() {
  local fm
  local -a both_list
  lines_into both_list "$4"$'\n'"$5"
  call full "$2" "$work/ft"
  ft=$status
  call full "$3" "$work/fm"
  fm=$status
  if ! is_judgeable "$ft" || ! is_judgeable "$fm"; then
    control_failed=1 why="the full member is answered $ft for $project's object and $fm for a missing one, which no rule judges, so its control was not read"
  elif ! alike "$ft" "$work/ft" "$fm" "$work/fm" ${both_list[@]+"${both_list[@]}"}; then
    can=1
  else
    why="the full member is answered $project's object as a missing one is ($ft), so the row could not fail"
  fi
}
as_missing_member() {
  local label=$1 rt rm marker
  local -a t_list both_list
  lines_into t_list "$4"
  lines_into both_list "$4"$'\n'"$5"
  call restricted "$2" "$work/rt"
  rt=$status
  call restricted "$3" "$work/rm"
  rm=$status
  if ! is_judgeable "$rt" || ! is_judgeable "$rm"; then
    unproven "$label: the member is answered $rt, and $rm for a missing one, which no rule judges"
    return
  fi
  member_marker "$label" "$work/rt" ${t_list[@]+"${t_list[@]}"} || return 0
  if [ -n "$marker" ]; then
    fail "$label: the member is answered $rt holding $(shown "$marker"), a row of $project"
  elif ! alike "$rt" "$work/rt" "$rm" "$work/rm" ${both_list[@]+"${both_list[@]}"}; then
    fail "$label: the member is answered $rt for $project's object, otherwise than a missing one answered $rm"
  else
    held "$label" "$rt, as a missing one is answered"
  fi
}

# not_installed STATUS FILE: whether an answer is the GitHub proxy's for a repository the App is
# not installed on (503 not_installed_code).
not_installed() {
  [ "$1" = 503 ] && [ "$(jq -r 'if type == "object" then .code // empty else empty end' "$2" 2>/dev/null)" = "$not_installed_code" ]
}
# proxy_answer STATUS FILE: whether an answer of the GitHub proxy is one a rule reads: a status
# some rule judges, or the answer for a repository the App is not installed on.
proxy_answer() { is_judgeable "$1" || not_installed "$1" "$2"; }

# proxy TARGET MISSING NAME: a GitHub proxy read of the project's repository (TARGET), beside the
# same read of one the App is not installed on (MISSING, the repository's name in NAME's place).
# The member must be answered as that missing one is, its name masked, or refused 403; any other
# answer told the member what the App reads of the project's repository. It can fail only where
# the full member's read of the project's repository is answered, other than 403, and the missing
# one's is the proxy's answer for a repository the App is not installed on. The proxy passes
# GitHub's own status through, and GitHub answers 403 to an App that lacks a permission: the
# member, on a build with no read check, is answered that 403 too, which their half takes as a
# refusal, so a full member's 403 is no read that could fail. The proxy reads the browser's
# person alone, so the full member reads as SWEEP_FULL_COOKIE's person; without it the full
# member's read is answered as no one's, and cannot fail.
proxy_full() {
  local fm cookie_why="the proxy reads the browser's person alone, whom SWEEP_FULL_COOKIE names"
  call "$full_browser" "$2" "$work/ft"
  ft=$status
  call "$full_browser" "$3" "$work/fm"
  fm=$status
  if ! not_installed "$fm" "$work/fm"; then
    control_failed=1 why="the full member is answered $fm for a repository the App is not installed on, not the proxy's own answer for one (503 $not_installed_code), so its control was not read"
  elif not_installed "$ft" "$work/ft"; then
    why="the full member is answered $ft for $project's repository as for one the App is not installed on ($fm), so the read could not fail: $cookie_why"
  elif ! is_judgeable "$ft"; then
    control_failed=1 why="the full member is answered $ft for $project's repository, which no rule judges, so its control was not read"
  elif [ "$ft" = 403 ]; then
    why="the full member is answered 403 for $project's repository, which GitHub answers an App that lacks a permission and the member's half takes as a refusal, so the read could not fail"
  else
    can=1
  fi
}
proxy_member() {
  local label=$1 name=$4 rt rm marker
  call restricted "$2" "$work/rt"
  rt=$status
  call restricted "$3" "$work/rm"
  rm=$status
  if ! proxy_answer "$rt" "$work/rt" || ! proxy_answer "$rm" "$work/rm"; then
    unproven "$label: the member is answered $rt, and $rm for a repository the App is not installed on, which no rule judges"
  elif alike "$rt" "$work/rt" "$rm" "$work/rm" "$name" no-such-repo; then
    held "$label" "$rt, as a repository the App is not installed on"
  elif [ "$rt" = 403 ]; then
    # A refusal of the project's repository alone, read for rows of the project first.
    member_marker "$label" "$work/rt" "$name" || return 0
    if [ -n "$marker" ]; then
      fail "$label: the member is answered $rt for $project's repository, where one the App is not installed on is answered $rm"
    else
      held "$label" 403
    fi
  else
    fail "$label: the member is answered $rt for $project's repository, where one the App is not installed on is answered $rm"
  fi
}

# agent_events FILE: the events of the agent frames an agent probe printed, joined by ", ".
agent_events() {
  jq -R -r -s '[split("\n")[] | fromjson? | select(type == "object" and .type == "agent") | .event // "untyped"]
    | join(", ")' "$1" 2>/dev/null
}
# no_one_answers FILE: whether an agent probe printed exactly one frame, the verdict that no one
# answers on the session's control subject, as a session the caller may not see is answered.
no_one_answers() {
  jq -R -s -e '[split("\n")[] | select(length > 0)] as $lines
    | ($lines | length) == 1 and ($lines[0] | fromjson? // null | type == "object"
      and .type == "agent" and .event == "responder" and .data.responding == false)' "$1" >/dev/null 2>&1
}
# agent_answer WHO FILE SESSION: the probe's answer to WHO's agent view of SESSION over
# /ws/live, its frames in FILE, `agent_exit` its exit.
agent_answer() {
  bun "$socket_probe" agent "$origin" "$work/$1.h" "$3" 1 >"$2" 2>&1
  agent_exit=$?
}

# agent_view SESSION: the live socket's agent view of SESSION, a session of the project alone (one
# conversation as it is spoken, over GET /ws/live). The member must be answered as for a session
# no one answers for, the one frame that says so and nothing after it, or refused the upgrade. It
# can fail where the full member, as SWEEP_FULL_COOKIE's person (the view reads the person scope),
# is answered more than that.
agent_view_full() {
  agent_answer "$full_browser" "$work/agent-full" "$2"
  ft=socket
  if [ "$agent_exit" != 0 ] || ! grep -q '^{' "$work/agent-full" || grep -q '^closed ' "$work/agent-full"; then
    control_failed=1 why="the full member's view was answered $(frames_of "$work/agent-full"), so its control was not read"
  elif ! no_one_answers "$work/agent-full"; then
    can=1
  else
    why="the full member is answered that no one answers for it too"
  fi
}
agent_view_member() {
  local label=$1 answer
  # Whether the view can fail a runner reads from the fills: the project's session answered the
  # full member when they were made, and may answer otherwise since, which the member's view cannot
  # tell, so the line says when.
  [ "$mode" != runner ] || [ "$can" != 1 ] || can_text='could fail when the fills were made'
  agent_answer restricted "$work/agent-member" "$2"
  answer=$(head -n 1 "$work/agent-member" | cut -d ' ' -f 1-2)
  refusal_body "$work/agent-member" "$work/agent-body"
  if [ -n "$(marker_in "$work/agent-body")" ]; then
    fail "$label: the member is answered $(frames_of "$work/agent-member"), holding $(shown "$(marker_in "$work/agent-body")"), a row of $project"
  elif [ "$agent_exit" != 0 ] || grep -q '^closed ' "$work/agent-member"; then
    unproven "$label: the member's view was answered $(frames_of "$work/agent-member")"
  else
    case $answer in
      'http 403' | 'http 404') held "$label" "the member's upgrade is refused: $answer" ;;
      'http '*) unproven "$label: the member's upgrade is answered $answer" ;;
      *)
        if no_one_answers "$work/agent-member"; then
          held "$label" 'no one answers for it'
        elif grep -q '^{' "$work/agent-member"; then
          fail "$label: the member is sent more of the session than that no one answers for it: $(agent_events "$work/agent-member")"
        else
          unproven "$label: the member's view was answered $(frames_of "$work/agent-member")"
        fi
        ;;
    esac
  fi
}

# refusal_body FILE OUT: the answer a socket probe printed, without the `http <status> ` its
# first line starts with when the upgrade was refused, so the markers read the server's body as
# the server wrote it.
refusal_body() {
  sed '1s/^http [0-9][0-9][0-9] //' "$1" >"$2" || unknown 'could not read a socket answer'
}

# not_filled LABEL WHY: a row the sweep could not fill, which leaves the run UNKNOWN.
not_filled() {
  unfilled=$((unfilled + 1))
  printf 'not filled %s: %s\n' "$1" "$2"
  printf '%s is not filled: %s\n' "$1" "$2" >>"$work/unknown" || unknown 'could not record an unfilled row'
}

# The missing side of each kind: an id, a key or a name nothing holds.
readonly none_uuid=00000000-0000-4000-8000-000000000000

# fill KIND PATTERN sets fill_target and fill_missing to the values KIND's wildcard takes in a
# row of PATTERN, fill_owned to 1 when the project owns KIND, and fill_why to why the target is
# empty when the lookup found none. A repository is the one only the project's issues resolve to
# (proxy_repo), since one a project the member sees resolves to as well may answer the member.
fill() {
  fill_owned=1 fill_why="the lookup found no $1 of $project" fill_missing=$none_uuid fill_target=''
  case $1 in
    project) fill_target=$project fill_missing=ZZNONE ;;
    issue)
      fill_missing=ZZNONE-1
      case $2 in
        */messages/\{id\}*) fill_target=$message_issue ;;
        */handoffs/\{phase\}*) fill_target=$handoff_issue ;;
        *) fill_target=$issue ;;
      esac
      ;;
    document)
      case $2 in
        # A document's own asks, comments, events and subscribers are a project document's: an
        # issue's documents answer them 400 ARTIFACT_LINKED and take them through the issue.
        /api/v1/artifacts/\{id\}/asks | /api/v1/artifacts/\{id\}/comments | /api/v1/artifacts/\{id\}/events | \
          /api/v1/artifacts/\{id\}/subscribers)
          fill_target=$pdoc fill_why="the lookup found no document of $project itself" ;;
        *) fill_target=$doc ;;
      esac
      ;;
    document-slug)
      fill_missing=no-such-document
      case $2 in
        /api/v1/issues/*) fill_target=$doc_slug ;;
        /api/v1/projects/*) fill_target=$pdoc_slug fill_why="the lookup found no document of $project itself" ;;
        *) fill_target=$conversation_slug fill_why=$conversation_why ;;
      esac
      ;;
    document-version) fill_target=1 fill_missing=1 ;;
    block) fill_target=$block fill_missing=b-nosuchblock ;;
    ask) fill_target=$ask ;;
    comment) fill_target=$comment ;;
    message) fill_target=$message ;;
    handoff) fill_target=$handoff ;;
    handoff-phase) fill_target=$handoff_phase fill_missing=$handoff_phase ;;
    near-copy-check) fill_target=$near_copy ;;
    going-run) fill_target=$going_run fill_why="the full member's question was refused: $going_why" ;;
    session)
      fill_missing=ses-nosuchsession
      case $2 in
        # A file of the conversation of a session that worked in the project alone, which the
        # member, no party to it, must be answered as missing.
        /api/v1/agents/\{session_id\}/artifacts/*) fill_target=$conversation_session fill_why=$conversation_why ;;
        # The session of the project alone the direct message is to, whose ids mark its rows.
        /api/v1/agents/\{session_id\}/messages)
          fill_owned=0 fill_target=$dm_session
          fill_why="no session the lookup read in $project alone has a direct message the full member reads"
          ;;
        *) fill_owned=0 fill_target=$session fill_why="no object of $project the lookup read names a session" ;;
      esac
      ;;
    person) fill_owned=0 fill_target=$person fill_missing=nobody@example.com fill_why="no object of $project the lookup read names a person" ;;
    repository-owner)
      fill_owned=0 fill_target=${proxy_repo%%/*} fill_missing=${proxy_repo%%/*}
      fill_why="no repository only $project's issues resolve to"
      ;;
    repository-name)
      fill_owned=0 fill_target=${proxy_repo#*/} fill_missing=no-such-repo
      fill_why="no repository only $project's issues resolve to"
      ;;
    priority) fill_owned=0 fill_target=$priority fill_missing=no-such-priority fill_why='the full member reads no priority' ;;
    broadcast) fill_owned=0 fill_target=$broadcast fill_why='the full member reads no broadcast' ;;
    delivery-run) fill_owned=0 fill_target=$delivery_run fill_missing=999999999999 fill_why='the full member reads no delivery run' ;;
    *) fill_why="the sweep fills no {$1} wildcard" ;;
  esac
  # A repository that names no owner is none.
  case $1 in repository-*) [ -n "$proxy_repo" ] || fill_target='' ;; esac
}

# query_kind PATTERN PARAM prints how a list row's query parameter is filled: a kind of object,
# `skip` for one that names no object of a project, or nothing for one the sweep does not know.
query_kind() {
  case "$1 $2" in
    '/api/v1/block-links from') echo document-refs ; return ;;
    '/api/v1/block-links to') echo skip ; return ;;
    '/api/v1/references from' | '/api/v1/references to' | '/api/v1/reference-targets ref') echo issue-ref ; return ;;
    '/api/v1/references/questions id') echo comment ; return ;;
    '/api/v1/search q' | '/api/v1/search/results q') echo title ; return ;;
  esac
  case $2 in
    project | number_project) echo project ;;
    issue | parent | ref) echo issue ;;
    artifact) echo document ;;
    repo) echo repository ;;
    # A session or a person is no project's node, but a list a filter by one names reads rows of
    # the projects it worked in: a filter by a session is filled with a session of the project
    # alone, which the member must be answered as one nobody holds, and a filter by a person with
    # a person of the project, whose answer the markers read. `session` is the session a caller
    # acts as, no filter. The rest name no object.
    author_session | parent_agent) echo project-session ;;
    assignee | approver | person | author) echo person ;;
    session) echo skip ;;
    advances | after | at | before | blocks | closed_before | closed_since | component | cursor | deployed | \
      download | exclude_issue | exclude_issues | from | ids | keywords_only | kind | label | limit | max_chars | \
      moved | offset | open | order | pinned | priority | q | rework | route_status | since | source_kind | state | \
      status | summary | text | thread | title | to | unlinked | updated_since | versions | wait) echo skip ;;
  esac
}

# sweep_queries PATTERN: each query parameter of the list PATTERN that names an object of a
# project, filled with the project's and with a missing one. A project, an issue, a document and a
# person fill a query as they fill a wildcard (`fill`): a list's pattern names no wildcard, so the
# pattern-bound fills of `fill` never apply.
sweep_queries() {
  local pattern=$1 param kind target missing strict t_fills m_fills fixed
  while read -r param; do
    [ -n "$param" ] || continue
    kind=$(query_kind "$pattern" "$param")
    strict=1 target='' missing=''
    case $kind in
      skip) continue ;;
      '')
        unproven "GET $pattern takes ?$param=, which the sweep does not classify"
        continue
        ;;
      project | issue | document | person)
        fill "$kind" "$pattern"
        target=$fill_target missing=$fill_missing strict=$fill_owned
        ;;
      issue-ref) [ -z "$issue" ] || [ -z "$ref_prefix" ] || target=$ref_prefix$issue missing=${ref_prefix}ZZNONE-1 ;;
      comment) target=$comment missing=00000000-0000-4000-8000-000000000002 ;;
      repository) target=$proxy_repo missing=${proxy_repo%%/*}/no-such-repo strict=0 ;;
      title) target=$issue_title missing='zz no such words zz' strict=0 ;;
      project-session) target=$dm_session missing=ses-nosuchsession ;;
      # Both ends of a block link must name a document: the issue's spec and the project's own.
      document-refs) [ -z "$issue" ] || [ -z "$pdoc_slug" ] || [ -z "$ref_prefix" ] || target=$issue ;;
    esac
    if [ -z "$target" ]; then
      case $kind in
        issue-ref | document-refs) not_filled "GET $pattern?$param=" "no node the lookup read names its reference, so the sweep cannot spell one" ;;
        *) not_filled "GET $pattern?$param=" "the lookup found no $kind of $project" ;;
      esac
      continue
    fi
    # A refusal names a reference's issue by its bare key too, so the fills mask that as well.
    t_fills=$target$'\n'$issue m_fills=$missing$'\n'ZZNONE-1
    case $kind in
      document-refs)
        row filled "GET $pattern?from=&to=" \
          "$pattern?from=$(uri "$ref_prefix$issue/spec")&to=$(uri "$ref_prefix$project/artifact/$pdoc_slug")" \
          "$pattern?from=$(uri "${ref_prefix}ZZNONE-1/spec")&to=$(uri "${ref_prefix}ZZNONE/artifact/no-such-document")" \
          1 "$project/artifact/$pdoc_slug"$'\n'"$issue" "ZZNONE/artifact/no-such-document"$'\n'ZZNONE-1
        ;;
      comment)
        row filled "GET $pattern?kind=comment&id=" "$pattern?kind=comment&id=$(uri "$target")" \
          "$pattern?kind=comment&id=$(uri "$missing")" 1 "$target" "$missing"
        ;;
      issue-ref)
        row filled "GET $pattern?$param=" "$pattern?$param=$(uri "$target")" "$pattern?$param=$(uri "$missing")" \
          1 "$t_fills" "$m_fills"
        ;;
      *)
        # A search answers nothing but 400 without its text, so its filters go with the title.
        fixed=''
        case "$pattern $param" in
          '/api/v1/search q' | '/api/v1/search/results q') ;;
          '/api/v1/search '* | '/api/v1/search/results '*) [ -z "$issue_title" ] || fixed="q=$(uri "$issue_title")&" ;;
        esac
        row filled "GET $pattern?${fixed:+q=<title>&}$param=" "$pattern?$fixed$param=$(uri "$target")" \
          "$pattern?$fixed$param=$(uri "$missing")" "$strict" "$target" "$missing"
        ;;
    esac
  done <<EOF
$(awk -v p="$pattern" '$1 == p { print $2 }' "$work/params")
EOF
}

# sweep_filled PATTERN: a row whose path names objects, each filled from the lookup.
sweep_filled() {
  local pattern=$1 wildcard kind target=$1 missing=$1 strict=0 t_fills='' m_fills=''
  for wildcard in $(wildcards_of "$pattern"); do
    kind=$(kind_of "$pattern" "$wildcard")
    fill "$kind" "$pattern"
    if [ -z "$fill_target" ]; then
      not_filled "GET $pattern" "{$wildcard}: $fill_why"
      return
    fi
    target=${target//"{$wildcard}"/$(uri "$fill_target")}
    missing=${missing//"{$wildcard}"/$(uri "$fill_missing")}
    if [ "$fill_target" != "$fill_missing" ]; then
      t_fills=$t_fills$fill_target$'\n'
      m_fills=$m_fills$fill_missing$'\n'
    fi
    [ "$fill_owned" = 0 ] || strict=1
  done
  if answers_as_missing "$pattern"; then
    row as_missing "GET $pattern" "$target" "$missing" "$t_fills" "$m_fills"
  else
    row filled "GET $pattern" "$target" "$missing" "$strict" "$t_fills" "$m_fills"
  fi
}

# route_drift: the deployment's own route index (GET /api/v1, read as the run's own caller) may
# list no GET route the route index the sweep walks (routes.txt) lacks: the sweep never called
# one it does, so it cannot say that route kept the project from the member, and the run is
# UNKNOWN, naming it. A test hook (a `_test` path) is mounted only under DISPATCH_TEST_HOOKS, which
# no deployment sets. An index that lists no route at all says nothing either way.
route_drift() {
  local who=restricted listed
  [ "$mode" != fills ] || who=full
  call "$who" /api/v1 "$work/route-index"
  if [ "$status" != 200 ] ||
    ! jq -e '.routes | type == "array" and length > 0' "$work/route-index" >/dev/null 2>&1; then
    unproven "GET /api/v1: the deployment's route index answered $status and listed no route, so the sweep cannot tell whether it lists a GET route routes.txt lacks"
    return
  fi
  jq -r '.routes[] | select(.method == "GET") | .path | select(type == "string")' "$work/route-index" |
    sort -u >"$work/listed-routes"
  awk '$1 == "GET" { print $2 }' "$routes" | sort -u >"$work/swept-routes"
  comm -23 "$work/listed-routes" "$work/swept-routes" | grep -v -e '/_test/' -e '/_test$' >"$work/unswept-routes"
  while IFS= read -r listed; do
    unproven "GET $listed: the deployment lists it and the route index routes.txt does not, so the sweep did not call it"
  done <"$work/unswept-routes"
}

# sweep_routes: every GET row of the route index, held to the deployment's own (route_drift), then
# the reads no row of it makes: the direct message by the routes that read one message, the live
# socket's agent view of its session, and the GitHub proxy, each read filled with the repository
# only the project's issues resolve to.
sweep_routes() {
  local method pattern suffix shape path proxy_sha=0123456789abcdef0123456789abcdef01234567
  # Each GET path's query parameters, from the API document, one `path param` line each.
  jq -r '.paths | to_entries[] | .key as $p | .value.get.parameters[]? | select(.in == "query") | "\($p) \(.name)"' \
    "$openapi" >"$work/params" 2>/dev/null || unknown "the API document $openapi holds no paths jq reads"
  [ -s "$work/params" ] || unknown "the API document $openapi names no query parameter, so it is not the document this sweep reads"
  rows=0
  while read -r method pattern _ || [ -n "${method:-}" ]; do
    [ "$method" = GET ] || continue
    rows=$((rows + 1))
    [ -n "$pattern" ] || unknown 'the route index holds a GET row with no path'
    case $pattern in
      *'{'*) sweep_filled "$pattern" ;;
      *)
        row list "GET $pattern" "$pattern"
        sweep_queries "$pattern"
        ;;
    esac
  done <"$routes"
  [ "$rows" -gt 0 ] || unknown "the route index $routes holds no GET row"
  route_drift
  for suffix in '' /blocks; do
    if [ -z "$dm_message" ]; then
      not_filled "GET /api/v1/messages/{id}$suffix (a direct message)" "the lookup found no direct message the full member reads"
      continue
    fi
    row filled "GET /api/v1/messages/{id}$suffix (a direct message)" "/api/v1/messages/$(uri "$dm_message")$suffix" \
      "/api/v1/messages/$none_uuid$suffix" 1 "$dm_message" "$none_uuid"
  done
  if [ -n "$dm_session" ]; then
    row agent_view '/ws/live agent view of {session_id}' "$dm_session"
  else
    not_filled '/ws/live agent view of {session_id}' "no session the lookup read in $project alone has a direct message the full member reads"
  fi
  while IFS= read -r shape; do
    if [ -z "$proxy_repo" ]; then
      not_filled "GET /api/github/rest/repos/{owner}/{repo}/$shape" "no repository only $project's issues resolve to"
      continue
    fi
    path=$shape
    path=${path//\{number\}/1}
    path=${path//\{sha\}/$proxy_sha}
    row proxy "GET /api/github/rest/repos/{owner}/{repo}/$shape" \
      "/api/github/rest/repos/$(uri "${proxy_repo%%/*}")/$(uri "${proxy_repo#*/}")/$path" \
      "/api/github/rest/repos/$(uri "${proxy_repo%%/*}")/no-such-repo/$path" "${proxy_repo#*/}"
  done <<EOF
$proxy_reads
EOF
}
