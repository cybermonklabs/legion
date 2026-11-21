# packages/envoy/scripts/lib/legacy-subjects-gate-judge.jq
#
# The judgement of legacy-subjects-gate.sh, which runs this program with jq over one document of
# what it read: the listener pods it found and what each one's log last said of its counter, each
# listener named outside the cluster and its /metrics, the lines the NATS Job printed
# (lib/legacy-subjects-gate-nats.sh), the times the reading began and ended, and the earlier
# reading it is compared with, or null. It reads nothing itself and answers {verdict, window, lines,
# record}: `shut`, `unknown`, `clean` or `open`, the seconds a second reading must come after the
# first, the lines the gate prints, and the record it writes.

include "legacy-subjects-gate-forms";

# The longest a listener's newest log line of its counter may come before the reading began: the
# listener logs one every minute (cmd/listener startLegacySubjectsLog), so an older one is a
# listener that stopped logging it.
def log_line_bound: 180;

# How much later than the window's start the stream's first message may be at a second reading:
# the stream expires each message at its max_age, so its first message is the first published
# after that moment, which on a production bus is moments later; an hour allows a quiet hour.
def first_message_slack: 3600;

# metrics reads a listener's metrics text: whether it declares the old-form counter, the counter's
# sample per label set, the start-time gauge's one sample (null when absent), and the lines that
# name the counter or the gauge and could not be read, which make the text unreadable.
def metrics:
  split("\n") as $lines
  | {
      counter: any($lines[]; . == "# TYPE envoy_listener_legacy_subjects_total counter"),
      counts: ([$lines[]
        | capture("^envoy_listener_legacy_subjects_total(?<labels>\\{[^}]*\\})? (?<n>[0-9]+)$")
        | {key: (.labels // "{}"), value: (.n | tonumber)}] | from_entries),
      starts: [$lines[] | capture("^envoy_listener_start_time_seconds (?<n>[0-9]+)$") | .n | tonumber],
      bad: [$lines[]
        | select(test("^envoy_listener_(legacy_subjects_total|start_time_seconds)"))
        | select(test("^envoy_listener_legacy_subjects_total(\\{[^}]*\\})? [0-9]+$") or test("^envoy_listener_start_time_seconds [0-9]+$") | not)]
    }
  | .start = (if (.starts | length) == 1 then .starts[0] else null end);

# hours is a number of seconds as hours, one decimal.
def hours: (. / 360 | floor) / 10;

# last_line is a text's last line that is not empty, or null.
def last_line: split("\n") | map(select(length > 0)) | last;

# cli_failure names a Job line's failed nats CLI read: its exit status and its last error line.
def cli_failure: "the nats CLI exited \(.rc): \((.err | last_line) // "no error text" | printable)";

# read_name names the read a Job line made, as the gate's lines name it.
def read_name:
  if .leg == "stream_info" then "the request $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS"
  elif .leg == "consumers" then "the request $JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset \(.arg | printable)"
  elif .leg == "subjects" then (.arg | split(" ")) as $a
    | "the request $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS for the subjects under \($a[0] | printable) at offset \($a[1] // "?" | printable)"
  elif .leg == "user_info" then "the request $SYS.REQ.USER.INFO"
  elif .leg == "interest_keys" then "nats kv ls envoy_interests"
  else .leg | printable
  end;

# answer reads one Job line's standard output as the JetStream API answer of type $kind: the
# parsed answer, or {problem} naming the read and why it is not one (the read's exit status, its
# error text, an answer that is no JSON object, of another type, or carrying the server's error).
# The parameter is not called $type: jq binds a parameter's name as a filter too, which would
# stand in for the builtin type inside this body.
def answer($kind):
  read_name as $read
  | if .rc != 0 then {problem: "\($read): \(cli_failure)"}
    elif (.out | length) == 0 then {problem: ("\($read) was answered with nothing" + (if (.err | length) > 0 then ": " + (.err | last_line | printable) else "" end))}
    else (.out | try fromjson catch null) as $a
      | if $a == null then {problem: "\($read): the answer is no JSON"}
        elif ($a | type) != "object" then {problem: "\($read): the answer is a JSON \($a | type), not an object"}
        elif ($a.error | type) == "object" then {problem: ("\($read): the server answered \($a.error.code // "?" | printable): \($a.error.description // "" | printable)")}
        elif $a.type != $kind then {problem: ("\($read): the answer is of type \($a.type // "none" | printable), not \($kind)")}
        else {value: $a}
        end
    end;

# consumer_page is a consumer list page's answer, its consumers held to the shape the judgement
# reads: a list of objects, each with a name and a configuration.
def consumer_page:
  read_name as $read
  | answer("io.nats.jetstream.api.v1.consumer_list_response")
  | if .problem then .
    elif ([.value.total, .value.offset] | any(type != "number"))
      or (.value.consumers // [] | type) != "array"
      or any(.value.consumers // [] | .[]; type != "object" or (.name | type) != "string" or ((.config // {}) | type) != "object")
    then {problem: "\($read): the answer is no list of consumers"}
    else .
    end;

# subject_page is a stream info answer listing subjects, held to the shape the judgement reads: a
# count and an offset, and the subjects as a map, absent where none matched.
def subject_page:
  read_name as $read
  | answer("io.nats.jetstream.api.v1.stream_info_response")
  | if .problem then .
    elif (.value.state | type) != "object" or ((.value.state.subjects // {}) | type) != "object"
      or ([.value.total, .value.offset] | any(type != "number"))
    then {problem: "\($read): the answer is no list of subjects"}
    else .
    end;

# paging_problem is why the pages of one list, in the order they were read, are not one reading
# of one list, or null: the server's count differs between them, or a page does not start at the
# last item of the page before (the Job asks each page from there, and the server sorts the list
# by name), so the list moved between two reads and an item may have been passed over unseen.
# names is each page's item names, in order.
def paging_problem(names):
  . as $pages
  | ([$pages[] | .total] | unique) as $totals
  | if ($totals | length) > 1 then "the server's count changed between its pages (\($totals | map(tostring) | join(", "))), so the list changed while it was read"
    else first(range(1; $pages | length) as $i
        | $pages[$i - 1] as $p
        | $pages[$i] as $q
        | ($p | names) as $pn
        | ($q | names) as $qn
        | (if ($pn | length) > 1 then $p.offset + ($pn | length) - 1 else $p.offset + ($pn | length) end) as $want
        | if $q.offset != $want then "the page at offset \($q.offset) does not start where the page at offset \($p.offset) ended (\($want))"
          elif ($pn | length) > 1 and $qn[0] != $pn[-1] then "the page at offset \($q.offset) begins with \($qn[0] // "nothing" | printable), not \($pn[-1] | printable), the last of the page before, so the list moved between the two reads and an item may have been passed over"
          else empty
          end) // null
    end;

# read_listener judges one listener's reading, the listener named $id: {id, unknown} when the read
# or its counter cannot be judged, else {id} with $fields, its start and its counts.
def read_listener($id; $fields):
  if (.read.ok | not) then {id: $id, unknown: "\($id): \(.read.error // "no answer" | printable)"}
  else (.read.text | metrics) as $m
    | if ($m.counter | not) then {id: $id, unknown: "\($id) serves no envoy_listener_legacy_subjects_total: its build predates the old-form counter, so what it was given cannot be read"}
      elif ($m.bad | length) > 0 then {id: $id, unknown: "\($id): the line \($m.bad[0] | printable) cannot be read"}
      elif ($m.starts | length) > 1 then {id: $id, unknown: "\($id) serves \($m.starts | length) start-time samples"}
      else {id: $id} + $fields + {start: $m.start, counts: $m.counts}
      end
  end;

# same_listener($l) says whether this listener reading reads the listener $l does: the same pod's
# container, or the same listener named outside the cluster.
def same_listener($l):
  if $l.kind == "pod" then .kind == "pod" and .uid == $l.uid and .container == $l.container
  else .kind == "outside" and .name == $l.name end;

# old_filters is a consumer's filters that only an old-form name matches: old-form filters whose
# new-form twin the consumer does not also filter.
def old_filters:
  ([.config.filter_subject // empty] + (.config.filter_subjects // [])) as $filters
  | [$filters[] | select(type == "string") | select(old_form) | select(has_twin($filters) | not)];

# rfc3339 is a JetStream time (RFC 3339, nanoseconds or none) in Unix seconds, or null.
def rfc3339: if type == "string" then (sub("\\.[0-9]+"; "") | try fromdateiso8601 catch null) else null end;

. as $in
| ($in.job.lines // []) as $jl
| ($jl | map(select(.leg == "end")) | length > 0) as $whole

# --- the listeners --------------------------------------------------------------------------------
# A listener in the cluster is read from its log's newest line of its counter (the gate's read of
# the pod), which must be recent: a listener logs one every minute.
| ($in.pods | map(
    if .read.ok and ((.read.at | type) != "number" or .read.at < $in.started_at - log_line_bound) then
      .read = {ok: false, error: "its newest \"listener legacy subjects\" log line was logged at \(.read.at | if type == "number" then todate else "an unread time" end), more than \(log_line_bound / 60) minutes before this reading began, though a listener logs one every minute: its counter now cannot be read"}
    else . end)) as $pods
| ([$pods[] | read_listener("listener pod \(.ns)/\(.pod) container \(.container)" | printable; {kind: "pod", uid, container, restarts, machine, read_at: .read.at})]
   + [$in.outside[] | read_listener("listener \(.name) at \(.url)" | printable; {kind: "outside", name, url, machine: .name, read_at: $in.started_at})]) as $listeners
| ($pods | map(select(.running | not))) as $notrunning

# --- the Job's reads ------------------------------------------------------------------------------
| ($jl | map(select(.leg == "stream_info")) | first // null) as $streamline
| (if $streamline == null then {problem: "the Job printed no stream read"}
   else ($streamline | answer("io.nats.jetstream.api.v1.stream_info_response")
     | if .value and ((.value.config | type) != "object" or (.value.state | type) != "object") then {problem: "the request $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS: the answer holds no configuration and state"} else . end)
   end) as $stream
| ($jl | map(select(.leg == "consumers") | consumer_page)) as $consumerpages
| (($consumerpages | length) > 0 and all($consumerpages[]; .value)) as $consumersread
| (if $consumersread then [$consumerpages[] | .value] | paging_problem([.consumers // [] | .[] | .name]) else null end) as $consumerpaging
| ([$consumerpages[] | .value.consumers // [] | .[]] | unique_by(.name)) as $consumers
| ([$consumerpages[] | .value.missing // [] | .[] | tostring] | unique) as $missingconsumers
| ($jl | map(select(.leg == "subjects") | {family: (.arg | split(" ")[0]), page: subject_page})) as $subjectpages
| ($jl | map(select(.leg == "interest_keys")) | first // null) as $keysline
| (if $keysline == null then null
   else ($keysline.out | split("\n") | map(select(length > 0)))
   end) as $keys
| ($jl | map(select(.leg == "interest"))) as $records
| ($jl | map(select(.leg == "interest_unread"))) as $unreadkeys
# Each record read: gone (deleted between the list and the read, as a session's record is when the
# session ends), unknown with the reason, or the old-form topics it holds. The session's own agent
# subject in the old form is not counted: the listener writes it into every record itself until the
# old form is refused (the subscribe route, store.CountStoredLegacyTopics), so it names no caller
# still on the old form.
| [$records[] | . as $r
    | if .rc != 0 then
        if (.err | test("key not found")) then {key: .arg, gone: true}
        else {key: .arg, unknown: "the interest store's record \(.arg | printable): \(cli_failure)"}
        end
      else (.out | try fromjson catch null) as $v
        | if ($v | type) != "object" or ($v.topics | type) != "array" then {key: .arg, unknown: "the interest store's record \(.arg | printable) is no interest record"}
          else ("notifications.agent." + (($v.session_id | strings) // $r.arg)) as $own
            | {key: .arg, old: [$v.topics[] | select(type == "string") | select(old_form) | select(. != $own)]}
          end
      end] as $recordresults
| (if $keys == null then [] else [$keys[] | select(test("^[A-Za-z0-9_=./-]+$")) | . as $k | select(($records | map(select(.arg == $k)) | length) == 0)] end) as $missingrecords

# --- whom the reads ran as ------------------------------------------------------------------------
| ($jl | map(select(.leg == "user_info")) | first // null) as $userline
| ($jl | map(select(.leg == "credential")) | first // null) as $credline
| (if $userline != null and $userline.rc == 0 then ($userline.out | try fromjson catch null) else null end) as $userinfo
| (if ($userinfo | type) == "object" and ($userinfo.data | type) == "object" and ($userinfo.data.user | type) == "string"
   then {user: $userinfo.data.user, account: ($userinfo.data.account // null)} else null end) as $said
| (if $credline != null and $credline.rc == 0 and ($credline.out | length) > 0
   then {kind: $credline.arg, names: ($credline.out | rtrimstr("\n") | . as $text | try fromjson catch $text)} else null end) as $credential
| (if ($jl | length) == 0 then null
   elif $said then "the server answers that the NATS reads ran as user \($said.user | printable), account \($said.account // "?" | printable)"
   else ("the server did not say whom the NATS reads ran as ("
       + (if $userline == null then "the Job printed no such read"
          else ($userline | read_name) + ": " + ($userline | if .rc != 0 then cli_failure elif (.out | length) == 0 then "answered with nothing" + (if (.err | length) > 0 then ": " + (.err | last_line | printable) else "" end) else "the answer names no user" end)
          end)
       + "); "
       + (if $credential == null then "the credential's own name could not be read either: \(($credline.err // "" | last_line) // "the Job printed no read of it" | printable)"
          elif $credential.kind == "nkey" then "they ran with the listener's nkey seed, of the nkey user \($credential.names | printable)"
          elif ($credential.names | type) != "object" then "they ran with the listener's projected token, whose claims cannot be read"
          else "they ran with the listener's projected token, for \($credential.names.sub // "?" | printable) (audience \($credential.names.aud // "?" | if type == "array" then join(", ") else tostring end | printable))"
          end))
   end) as $identity

# --- the stream's window ------------------------------------------------------------------------
| ($stream.value.config.max_age // null) as $maxagens
| (if $maxagens == null then null else ($maxagens / 1000000000 | floor) end) as $maxage
| ([259200, ($maxage // 0), ($in.earlier.stream.max_age_seconds // 0)] | max) as $window
| ($stream.value.config.metadata.envoy_stream_id // null) as $streamid
| ($stream.value.state.first_ts // null) as $firstts
| ($firstts | rfc3339) as $first
| ([$consumers[] | select(.name | startswith("listener-")) | .config.inactive_threshold // 0 | select(type == "number" and . > 0)] | min) as $lapsens

# --- what each surface holds ----------------------------------------------------------------------
| [
    # The listeners.
    if $in.pods_error then {unknown: "the listeners in the cluster: \($in.pods_error | printable)"}
    elif ($in.pods | length) == 0 then {unknown: "no listener container runs in any namespace of the cluster (an image whose path ends legion/envoy running its own entrypoint, envoy-listener): a reading that found none read nothing"}
    else empty end,
    ($notrunning[] | {unknown: ("listener pod \(.ns)/\(.pod) container \(.container) is \(.state), not running: its counter cannot be read" | printable)}),
    ($listeners[] | select(.unknown) | {unknown: .unknown}),

    # The Job itself.
    if ($in.job.ok | not) then {unknown: "the NATS reads: \($in.job.error // "the Job did not run" | printable)"}
    elif ($whole | not) then {unknown: "the NATS reads: the Job's log ends before its last read, so what it read is not whole"}
    else empty end,

    # The stream: its info, and the identity a second reading compares.
    if $stream.problem then {unknown: "the stream's info: \($stream.problem)"}
    elif $streamid == null then {unknown: "the stream's info: its configuration carries no metadata envoy_stream_id, the identity a second reading holds it to"}
    else empty end,

    # The consumers.
    (if ($consumerpages | length) == 0 then {unknown: "the stream's consumers: the Job printed no consumer list"} else empty end),
    ($consumerpages[] | select(.problem) | {unknown: "the stream's consumers: \(.problem)"}),
    (if $consumerpaging then {unknown: "the stream's consumers: \($consumerpaging)"} else empty end),
    (if ($missingconsumers | length) > 0 then {unknown: "the stream's consumers: the server could not read \($missingconsumers | map(printable) | join(", "))"} else empty end),
    (if $consumersread then
       ($consumerpages | last | .value.total) as $total
       | if ($consumers | length) != $total then {unknown: "the stream's consumers: the list read \($consumers | length) distinct of the \($total) the server counts"} else empty end
     else empty end),
    (if $consumersread and ($consumers | map(select(.name | test("^legion-go-.+-dispatch$"))) | length) == 0 then
       {unknown: "the stream's consumers: no Legion daemon consumer (legion-go-<project>-dispatch) is on the stream, so the daemons' consumers were not read"}
     else empty end),
    ($consumers[]
      | . as $c
      | old_filters as $old
      | select(($old | length) > 0)
      | {shut: "consumer \($c.name | printable) filters \($old | map(printable) | join(", ")), which only an old-form name matches (delivered: consumer sequence \($c.delivered.consumer_seq // 0), stream sequence \($c.delivered.stream_seq // 0))", consumer: {name: $c.name, old: $old, delivered: ($c.delivered.consumer_seq // 0), stream_seq: ($c.delivered.stream_seq // 0)}}),
    (if $consumersread and all($consumers[]; old_filters | length == 0) then
       {pass: "no consumer of the stream's \($consumers | length) filters a name only the old form matches (\($consumers | map(select(.name | startswith("legion-go-"))) | length) of them are Legion daemons')"}
     else empty end),
    # Every listener that has served on the bus, by its durable (bus.ListenerDurable), bound now or
    # not: one whose counter this reading did not read is a reader the counter leg could not see.
    # A durable outlives its listener for its inactive_threshold, so a listener that is offline,
    # asleep or restarting is named here too.
    (if $consumersread then
       ([$listeners[] | select(.unknown | not) | .machine | strings | gsub(" "; "-")]) as $machines
       | $consumers[]
       | select(.name | startswith("listener-"))
       | (.name | ltrimstr("listener-")) as $m
       | select(($machines | index($m)) == null)
       | if .push_bound == true then
           {unknown: "listener machine \($m | printable) is bound to its durable \(.name | printable) on the bus, and no listener this reading read serves as that machine: name it with --listener \($m | printable)=<url>, or read it in the cluster"}
         else
           {unknown: "listener machine \($m | printable) has its durable \(.name | printable) on the bus and is not bound to it now (offline, asleep or restarting; last active \(.delivered.last_active // "never" | printable)), and no listener this reading read serves as that machine: what it counted while it ran cannot be read; read again while it runs, naming it with --listener \($m | printable)=<url> unless it runs in the cluster"}
         end
     else empty end),

    # The stream's subjects.
    ($subjectpages[] | select(.page.problem) | {unknown: "the stream's subjects under \(.family | printable): \(.page.problem)"}),
    (["notifications.dispatch.issue.>", "notifications.agent.>", "notifications.envoy.exceptions.notifications.agent.>", "notifications.envoy.kept.>"][]
      | . as $family
      | ($subjectpages | map(select(.family == $family))) as $pages
      | if ($pages | length) == 0 then {unknown: "the stream's subjects under \($family): the Job printed no read of them"}
        elif all($pages[]; .page.value) then
          ([$pages[] | .page.value] | paging_problem([.state.subjects // {} | keys[]])) as $paging
          | ([$pages[] | .page.value.state.subjects // {} | keys[]] | unique) as $subjects
          | ($pages | last | .page.value.total) as $total
          | if $paging then {unknown: "the stream's subjects under \($family): \($paging)"}
            elif ($subjects | length) != $total then {unknown: "the stream's subjects under \($family): the read holds \($subjects | length) of the \($total) the server counts"}
            elif ($subjects | length) == 0 and ($family == "notifications.dispatch.issue.>" or $family == "notifications.agent.>") then
              {unknown: "the stream's subjects under \($family): the stream holds none, which a stream every issue event and every send passes through never does, so the read saw nothing"}
            else ($subjects | map(select(old_form))) as $old
              | if ($old | length) > 0 then {shut: "the stream holds \($old | length) old-form subject(s) under \($family): \($old[:5] | map(printable) | join(", "))\(if ($old | length) > 5 then ", …" else "" end)", subjects: $old}
                else {pass: "the stream holds \($subjects | length) subject(s) under \($family), none in an old form"}
                end
            end
        else empty
        end),

    # The interest records.
    (if $keysline == null then {unknown: "the interest store: the Job printed no read of its keys"}
     elif $keysline.rc != 0 or ($keysline.err | test("(?i)violation|error")) then {unknown: "the interest store's keys: \($keysline | read_name): \($keysline | cli_failure)"}
     elif ($keys | length) == 0 then {unknown: "the interest store lists no record, which a store every registration writes to never does, so the read saw nothing"}
     else empty end),
    (if $keysline != null and $keysline.rc == 0 and ($keys | length) > 0 and ($unreadkeys | length) == 0 and ($missingrecords | length) == 0
        and all($recordresults[]; (.unknown | not) and ((.old // []) | length) == 0) then
       {pass: "no interest record of the \($recordresults | map(select(.gone | not)) | length) read holds an old-form topic beyond its session's own agent subject"}
     else empty end),
    ($unreadkeys[] | {unknown: "the interest store listed \(.out | printable), which is no key, and it was not read"}),
    ($missingrecords[] | {unknown: "the interest store's record \(. | printable) was listed and not read"}),
    ($recordresults[] | select(.unknown) | {unknown: .unknown}),
    ($recordresults[] | select((.old // []) | length > 0)
      | {shut: "the interest record of session \(.key | printable) holds \(.old | map(printable) | join(", ")), in an old form", record: {key: .key, old: .old}})
  ] as $found

# --- the comparison with the earlier reading -----------------------------------------------------
| (if $in.earlier == null then []
   else
     ($in.earlier) as $e
     | ($in.started_at - $e.finished_at) as $gap
     | [
         if $e.verdict == "shut" then {shut: "the first reading, taken \($e.read_at), was not clean: it held an old-form name; take a new first reading once none remains"}
         elif $e.verdict != "clean" then {unknown: "the first reading, taken \($e.read_at), was not clean: it could not read every surface; take a new first reading"}
         else empty end,
         if $gap < $window then {shut: "\($gap | hours) h passed between the first reading's end (\($e.finished_at | todate)) and this one; the stream keeps \($window | hours) h, so read again at or after \(($e.finished_at + $window) | todate)"} else empty end,
         # The stream must be the one the first reading read, and hold the whole window now, or
         # an old-form name published before it was made anew or purged cannot be seen.
         if $streamid != null and ($e.stream.id // null) == null then {unknown: "the first reading recorded no identity of the stream (envoy_stream_id), so this reading is not shown to read the same stream; take a new first reading"}
         elif $streamid != null and $e.stream.id != $streamid then {unknown: "the stream is not the one the first reading read: its envoy_stream_id was \($e.stream.id | printable) and is \($streamid | printable), so it was made anew between the readings and what it held then is gone; take a new first reading"}
         else empty end,
         if $stream.value == null then empty
         elif $first == null or $first <= 0 then {unknown: "the stream's first message time (state.first_ts \($firstts | tostring | printable)) is no time, so whether it holds the whole \($window | hours) h window cannot be read"}
         elif $first > $in.started_at - $window + first_message_slack then {unknown: "the stream holds messages only since \($first | todate), \(($in.started_at - $first) | hours) h before this reading, not the \($window | hours) h it keeps: it was purged or made anew within the window, so an old-form name published before then cannot be seen; read again at or after \(($first + $window - first_message_slack) | todate)"}
         else empty end,
         # A listener's durable outlives its listener for its inactive_threshold alone: past it, a
         # listener that served between the readings and then stopped may have left no durable to
         # name it, and would be in neither reading.
         if $lapsens != null and $gap > ($lapsens / 1000000000) then {unknown: "\($gap | hours) h passed between the readings, longer than a listener's durable outlives its listener (\($lapsens / 1000000000 | hours) h unbound, its inactive_threshold): a listener that served between them and stopped may have left no durable to name it; take a new first reading and read again sooner"} else empty end,
         ($listeners[] | select(.unknown | not) | . as $now
           | ($e.listeners // [] | map(select(same_listener($now))) | first // null) as $then
           | if $then == null then {unknown: "\($now.id) was not read at the first reading: it started after it, so its counter covers only part of the window"}
             elif $now.kind == "pod" and $then.restarts != $now.restarts then {unknown: "\($now.id) restarted between the readings (restart count \($then.restarts) then \($now.restarts)): its counter covers only the time since"}
             elif $now.start != null and $then.start != null and $now.start != $then.start then {unknown: "\($now.id) restarted between the readings: its start-time gauge read \($then.start) then \($now.start), so its counter covers only the time since \($now.start | todate)"}
             elif ($now.start == null) != ($then.start == null) then {unknown: "\($now.id) served a start-time gauge at one reading and not the other, so the two counts are not shown to be one process's"}
             elif $now.kind == "outside" and $now.start == null then {unknown: "\($now.id) runs in no pod and serves no start-time gauge (envoy_listener_start_time_seconds), so a restart between the readings, which resets its counter, cannot be seen"}
             elif ($now.read_at | type) == "number" and ($then.read_at | type) == "number" and $now.read_at - $then.read_at < $window then {shut: "\($now.id)'s counter was read \(($now.read_at - $then.read_at) | hours) h apart, at \($then.read_at | todate) and \($now.read_at | todate), less than the \($window | hours) h window; read again at or after \(($then.read_at + $window) | todate)"}
             else
               ((($now.counts | keys) + ($then.counts | keys)) | unique) as $labels
               | [$labels[] | . as $l | {label: $l, then: ($then.counts[$l] // 0), now: ($now.counts[$l] // 0)}] as $diff
               | if any($diff[]; .now > .then) then {shut: "\($now.id) counted old-form names since the first reading: \($diff | map(select(.now > .then) | "\(.label | printable) \(.then) to \(.now)") | join(", "))"}
                 elif any($diff[]; .now < .then) then {unknown: "\($now.id)'s count went down with no restart this reading can see: \($diff | map(select(.now < .then) | "\(.label | printable) \(.then) to \(.now)") | join(", "))"}
                 else {pass: "\($now.id) counted no old-form name between the readings"}
                 end
             end),
         ($e.listeners // [] | .[] | . as $then
           | select(any($listeners[]; select(.unknown | not) | same_listener($then)) | not)
           | {unknown: "\($then.id) served at the first reading and was not read now: what it counted after that reading cannot be read"})
       ]
   end) as $compared

| ($found + $compared) as $all
| ($all | map(select(.shut))) as $shut
| ($all | map(select(.unknown))) as $unknown
| (if ($shut | length) > 0 then "shut" elif ($unknown | length) > 0 then "unknown" elif $in.earlier == null then "clean" else "open" end) as $verdict
| {
    verdict: $verdict,
    window: $window,
    lines: (
      [if $identity then "IDENTITY: " + $identity else empty end]
      + [$listeners[] | select(.unknown | not) | "READ: \(.id): \(if .counts == {} then "no old-form name counted" else (.counts | to_entries | map("\(.key | printable) \(.value)") | join(", ")) end) since its start \(if .start then (.start | todate) else "(no start-time gauge)" end)"]
      + [$all[] | select(.pass) | "PASS: " + .pass]
      + [$shut[] | "GATE SHUT: " + .shut]
      + [$unknown[] | "UNKNOWN: " + .unknown]
    ),
    record: {
      gate: "legacy-subjects-gate",
      version: 2,
      context: $in.context,
      started_at: $in.started_at,
      finished_at: $in.finished_at,
      read_at: ($in.started_at | todate),
      verdict: (if $verdict == "open" then "clean" else $verdict end),
      compared_with: (if $in.earlier == null then null else $in.earlier.read_at end),
      identity: {said: $said, credential: $credential},
      stream: {max_age_seconds: $maxage, first_ts: $firstts, created: ($stream.value.created // null), id: $streamid},
      listeners: [$listeners[] | select(.unknown | not)],
      consumers_old: [$found[] | select(.consumer) | .consumer],
      subjects_old: [$found[] | select(.subjects) | .subjects[]],
      interests_old: [$found[] | select(.record) | .record],
      shut: [$shut[] | .shut],
      unknown: [$unknown[] | .unknown]
    }
  }
