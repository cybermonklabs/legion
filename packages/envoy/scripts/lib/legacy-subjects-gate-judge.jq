# packages/envoy/scripts/lib/legacy-subjects-gate-judge.jq
#
# The judgement of legacy-subjects-gate.sh, which runs this program with jq over one document of
# what it read: the listener pods it found and each one's /metrics, each listener named outside the
# cluster and its /metrics, the lines the NATS Job printed (lib/legacy-subjects-gate-nats.sh), the
# times the reading began and ended, and the earlier reading it is compared with, or null. It reads
# nothing itself and answers {verdict, window, lines, record}: `shut`, `unknown`, `clean` or
# `open`, the seconds a second reading must come after the first, the lines the gate prints, and the
# record it writes.

include "legacy-subjects-gate-forms";

# metrics reads a listener's /metrics text: whether it declares the old-form counter, the counter's
# sample per label set, the start-time gauge's one sample (null when absent), and the lines that
# name the counter or the gauge and could not be read, which make the scrape unreadable.
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

# answer reads one Job line's standard output as the JetStream API answer of type $type: the
# parsed answer, or {problem} naming why it is not one (the read's exit status, its error text, an
# answer that is no JSON, of another type, or carrying the server's error).
def answer($type):
  if .rc != 0 then {problem: cli_failure}
  elif (.out | length) == 0 then {problem: ("no answer" + (if (.err | length) > 0 then ": " + (.err | last_line | printable) else "" end))}
  else (.out | try fromjson catch null) as $a
    | if $a == null then {problem: "the answer is no JSON"}
      elif ($a.error | type) == "object" then {problem: ("the server answered \($a.error.code // "?"): \($a.error.description // "" | printable)")}
      elif $a.type != $type then {problem: ("the answer is of type \($a.type // "none" | printable), not \($type)")}
      else {value: $a}
      end
  end;

# read_listener judges one listener's /metrics read, the listener named $id: {id, unknown} when the
# read or its counter cannot be judged, else {id} with $fields, its start and its counts.
def read_listener($id; $fields):
  if (.read.ok | not) then {id: $id, unknown: "\($id): its /metrics could not be read: \(.read.error // "no answer" | printable)"}
  else (.read.text | metrics) as $m
    | if ($m.counter | not) then {id: $id, unknown: "\($id) serves no envoy_listener_legacy_subjects_total: its build predates the old-form counter, so what it was given cannot be read"}
      elif ($m.bad | length) > 0 then {id: $id, unknown: "\($id): /metrics line \($m.bad[0] | printable) cannot be read"}
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
  | [$filters[] | select(old_form) | select(has_twin($filters) | not)];

. as $in
| ($in.job.lines // []) as $jl
| ($jl | map(select(.leg == "end")) | length > 0) as $whole

# --- the listeners --------------------------------------------------------------------------------
| ([$in.pods[] | read_listener("listener pod \(.ns)/\(.pod) container \(.container)" | printable; {kind: "pod", uid, container, restarts, machine})]
   + [$in.outside[] | read_listener("listener \(.name) at \(.url)" | printable; {kind: "outside", name, url, machine: .name})]) as $listeners
| ($in.pods | map(select(.running | not))) as $notrunning

# --- the Job's reads ------------------------------------------------------------------------------
| ($jl | map(select(.leg == "stream_info")) | first // null) as $streamline
| (if $streamline == null then {problem: "the Job printed no stream read"} else ($streamline | answer("io.nats.jetstream.api.v1.stream_info_response")) end) as $stream
| ($jl | map(select(.leg == "consumers") | answer("io.nats.jetstream.api.v1.consumer_list_response"))) as $consumerpages
| (($consumerpages | length) > 0 and all($consumerpages[]; .value)) as $consumersread
| ([$consumerpages[] | .value.consumers // [] | .[]]) as $consumers
| ($jl | map(select(.leg == "subjects") | {family: (.arg | split(" ")[0]), page: answer("io.nats.jetstream.api.v1.stream_info_response")})) as $subjectpages
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
          else ("notifications.agent." + ($v.session_id // $r.arg)) as $own
            | {key: .arg, old: [$v.topics[] | select(type == "string") | select(old_form) | select(. != $own)]}
          end
      end] as $recordresults
| (if $keys == null then [] else [$keys[] | select(test("^[A-Za-z0-9_=./-]+$")) | . as $k | select(($records | map(select(.arg == $k)) | length) == 0)] end) as $missingrecords

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

    # The stream.
    if $stream.problem then {unknown: "the stream's info: \($stream.problem)"} else empty end,

    # The consumers.
    (if ($consumerpages | length) == 0 then {unknown: "the stream's consumers: the Job printed no consumer list"} else empty end),
    ($consumerpages[] | select(.problem) | {unknown: "the stream's consumers: \(.problem)"}),
    (if $consumersread then
       ($consumerpages | last | .value.total) as $total
       | if ($consumers | length) != $total then {unknown: "the stream's consumers: the list read \($consumers | length) of the \($total) the server counts"} else empty end
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
    # Every listener that serves on the bus, by its bound durable (bus.ListenerDurable): one whose
    # /metrics this reading did not read is a reader the counter leg could not see.
    (if $consumersread then
       ([$listeners[] | select(.machine) | .machine | gsub(" "; "-")]) as $machines
       | $consumers[]
       | select(.name | startswith("listener-"))
       | select(.push_bound == true)
       | (.name | ltrimstr("listener-")) as $m
       | select(($machines | index($m)) == null)
       | {unknown: "listener machine \($m | printable) is bound to its durable \(.name | printable) on the bus, and no listener this reading read serves as that machine: name it with --listener \($m | printable)=<url>, or read it in the cluster"}
     else empty end),

    # The stream's subjects.
    ($subjectpages[] | select(.page.problem) | {unknown: "the stream's subjects under \(.family | printable): \(.page.problem)"}),
    (["notifications.dispatch.issue.>", "notifications.agent.>", "notifications.envoy.exceptions.notifications.agent.>", "notifications.envoy.kept.>"][]
      | . as $family
      | ($subjectpages | map(select(.family == $family))) as $pages
      | if ($pages | length) == 0 then {unknown: "the stream's subjects under \($family): the Job printed no read of them"}
        elif all($pages[]; .page.value) then
          ([$pages[] | .page.value.state.subjects // {} | keys[]] | unique) as $subjects
          | ($pages | last | .page.value.total) as $total
          | if ($subjects | length) != $total then {unknown: "the stream's subjects under \($family): the read holds \($subjects | length) of the \($total) the server counts"}
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
     elif $keysline.rc != 0 or ($keysline.err | test("(?i)violation|error")) then {unknown: "the interest store's keys: \($keysline | cli_failure)"}
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
| ($stream.value.config.max_age // null) as $maxagens
| (if $maxagens == null then null else ($maxagens / 1000000000 | floor) end) as $maxage
| ([259200, ($maxage // 0), ($in.earlier.stream.max_age_seconds // 0)] | max) as $window
| (if $in.earlier == null then []
   else
     ($in.earlier) as $e
     | ($in.started_at - $e.finished_at) as $gap
     | [
         if $e.verdict == "shut" then {shut: "the first reading, taken \($e.read_at), was not clean: it held an old-form name; take a new first reading once none remains"}
         elif $e.verdict != "clean" then {unknown: "the first reading, taken \($e.read_at), was not clean: it could not read every surface; take a new first reading"}
         else empty end,
         if $gap < $window then {shut: "\($gap | hours) h passed between the first reading's end (\($e.finished_at | todate)) and this one; the stream keeps \($window | hours) h, so read again at or after \(($e.finished_at + $window) | todate)"} else empty end,
         ($listeners[] | select(.unknown | not) | . as $now
           | ($e.listeners // [] | map(select(same_listener($now))) | first // null) as $then
           | if $then == null then {unknown: "\($now.id) was not read at the first reading: it started after it, so its counter covers only part of the window"}
             elif $now.kind == "pod" and $then.restarts != $now.restarts then {unknown: "\($now.id) restarted between the readings (restart count \($then.restarts) then \($now.restarts)): its counter covers only the time since"}
             elif $now.start != null and $then.start != null and $now.start != $then.start then {unknown: "\($now.id) restarted between the readings: its start-time gauge read \($then.start) then \($now.start), so its counter covers only the time since \($now.start | todate)"}
             elif ($now.start == null) != ($then.start == null) then {unknown: "\($now.id) served a start-time gauge at one reading and not the other, so the two counts are not shown to be one process's"}
             elif $now.kind == "outside" and $now.start == null then {unknown: "\($now.id) runs in no pod and serves no start-time gauge (envoy_listener_start_time_seconds), so a restart between the readings, which resets its counter, cannot be seen"}
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
      [$listeners[] | select(.unknown | not) | "READ: \(.id): \(if .counts == {} then "no old-form name counted" else (.counts | to_entries | map("\(.key | printable) \(.value)") | join(", ")) end) since its start \(if .start then (.start | todate) else "(no start-time gauge)" end)"]
      + [$all[] | select(.pass) | "PASS: " + .pass]
      + [$shut[] | "GATE SHUT: " + .shut]
      + [$unknown[] | "UNKNOWN: " + .unknown]
    ),
    record: {
      gate: "legacy-subjects-gate",
      version: 1,
      context: $in.context,
      started_at: $in.started_at,
      finished_at: $in.finished_at,
      read_at: ($in.started_at | todate),
      verdict: (if $verdict == "open" then "clean" else $verdict end),
      compared_with: (if $in.earlier == null then null else $in.earlier.read_at end),
      stream: {max_age_seconds: $maxage, first_ts: ($stream.value.state.first_ts // null)},
      listeners: [$listeners[] | select(.unknown | not)],
      consumers_old: [$found[] | select(.consumer) | .consumer],
      subjects_old: [$found[] | select(.subjects) | .subjects[]],
      interests_old: [$found[] | select(.record) | .record],
      shut: [$shut[] | .shut],
      unknown: [$unknown[] | .unknown]
    }
  }
