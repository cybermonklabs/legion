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
# sample per label set, the start-time gauge's samples with the machine each names (its one label,
# machine, unescaped; null where it has none), the one start and its machine (null when there is no
# one sample), and the lines that name the counter or the gauge and could not be read, which make
# the text unreadable.
def metrics:
  split("\n") as $lines
  | "^envoy_listener_start_time_seconds(\\{machine=\"(?<m>([^\"\\\\]|\\\\.)*)\"\\})? (?<n>[0-9]+)$" as $start
  | {
      counter: any($lines[]; . == "# TYPE envoy_listener_legacy_subjects_total counter"),
      counts: ([$lines[]
        | capture("^envoy_listener_legacy_subjects_total(?<labels>\\{[^}]*\\})? (?<n>[0-9]+)$")
        | {key: (.labels // "{}"), value: (.n | tonumber)}] | from_entries),
      starts: [$lines[] | capture($start)
        | {value: (.n | tonumber), machine: (.m | if . == null then null else gsub("\\\\(?<c>.)"; if .c == "n" then "\n" else .c end) end)}],
      bad: [$lines[]
        | select(test("^envoy_listener_(legacy_subjects_total|start_time_seconds)"))
        | select(test("^envoy_listener_legacy_subjects_total(\\{[^}]*\\})? [0-9]+$") or test($start) | not)]
    }
  | .start = (if (.starts | length) == 1 then .starts[0].value else null end)
  | .machine = (if (.starts | length) == 1 then .starts[0].machine else null end);

# hours is a number of seconds as hours, one decimal.
def hours: (. / 360 | floor) / 10;

# last_line is a text's last line that is not empty, or null.
def last_line: split("\n") | map(select(length > 0)) | last;

# masked_urls is a NATS_URLS as a line prints it: in each URL, split at commas as the listener
# splits them (internal/config Load), everything before its last @ after the scheme, if it has one,
# is masked (masked_userinfo). printable masks a URL with a scheme in any text; this also masks one
# with none, which the listener's NATS client dials with its default scheme, so no line prints a
# credential a listener's environment holds.
def masked_urls:
  split(",")
  | map(sub("^(?<s>([A-Za-z][A-Za-z0-9+.-]*://)?)(?<i>[\\s\\S]*)@"; "\(.s)\(.i | masked_userinfo)@"))
  | join(",");

# cli_failure names a Job line's failed nats CLI read: its exit status and its last error line.
def cli_failure: "the nats CLI exited \(.rc): \((.err | last_line) // "no error text" | printable)";

# read_name names the read a Job line made, as the gate's lines name it.
def read_name:
  if .leg == "stream_info" then "the request $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS"
  elif .leg == "consumers" then "the request $JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS at offset \(.arg | printable)"
  elif .leg == "subjects" then (.arg | split(" ")) as $a
    | "the request $JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS for the subjects under \($a[0] | printable) at offset \($a[1] // "?" | printable)"
  elif .leg == "old_last" then "the request $JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS for the last message of \(.arg | printable)"
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
# or its counter cannot be judged, else {id} with $fields, its start, its counts and its machine.
# A listener's start-time gauge names its machine (label machine, its ENVOY_MACHINE_ID): a listener
# outside the cluster must name the machine the operator gave it, since only that shows the URL
# serves that machine and not another listener, and a pod that names one must name its container's
# ENVOY_MACHINE_ID; a pod that names none is held to its container's.
def read_listener($id; $fields):
  if (.read.ok | not) then {id: $id, unknown: "\($id): \(.read.error // "no answer" | printable)"}
  else (.read.text | metrics) as $m
    | if ($m.counter | not) then {id: $id, unknown: "\($id) serves no envoy_listener_legacy_subjects_total: its build predates the old-form counter, so what it was given cannot be read"}
      elif ($m.bad | length) > 0 then {id: $id, unknown: "\($id): the line \($m.bad[0] | printable) cannot be read"}
      elif ($m.starts | length) > 1 then {id: $id, unknown: "\($id) serves \($m.starts | length) start-time samples"}
      elif $fields.kind == "outside" and $m.machine == null then {id: $id, unknown: "\($id) names no machine on its start-time gauge (label machine), so its URL is not shown to serve machine \($fields.machine | printable)"}
      elif $m.machine != null and $fields.machine != null and $m.machine != $fields.machine then {id: $id, unknown: "\($id) serves machine \($m.machine | printable), not \($fields.machine | printable): it reads another listener than the one named"}
      else {id: $id} + $fields + {start: $m.start, counts: $m.counts, machine: ($m.machine // $fields.machine)}
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
| ($stream.value.state.last_seq // null) as $lastseq
# The last message of each old-form subject, as the Job read it by last_by_subj: {seq, time}, gone
# (no message found: its messages expired between the list and the read), or {problem}.
| ($jl | map(select(.leg == "old_last")
    | {key: .arg, value: (
        (.out | try fromjson catch null) as $raw
        | answer("io.nats.jetstream.api.v1.stream_msg_get_response") as $a
        | if ($raw | type) == "object" and ($raw.error | type) == "object" and ($raw.error.err_code == 10037 or $raw.error.code == 404) then {gone: true}
          elif $a.problem then {problem: $a.problem}
          elif ($a.value.message | type) != "object" or ($a.value.message.seq | type) != "number" then {problem: "\(read_name): the answer holds no message's stream sequence"}
          else {seq: $a.value.message.seq, time: ($a.value.message.time // null)}
          end)})
   | from_entries) as $oldlast
# The newest old-form message the stream holds, of those whose last message was read, and how many
# old-form subjects' last message was not: beyond the Job's cap (old_last_skipped), or a read that
# failed. These name what a reading found and decide no verdict: a first reading shuts on no
# old-form subject, and a second shuts on every one.
| ([$oldlast | to_entries[] | select(.value.seq) | {subject: .key, seq: .value.seq, time: .value.time}] | max_by(.seq)) as $newestold
| ($jl | map(select(.leg == "old_last_skipped")) | first // null) as $skippedline
| ({cap: ($skippedline.arg // null), skipped: ($skippedline.out // "0" | tonumber? // 0),
    failed: ([$oldlast[] | select(.problem)] | length)}) as $unreadlast

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
    elif ($lastseq | type) != "number" then {unknown: "the stream's info: it holds no last stream sequence (state.last_seq), the boundary a second reading reads old-form messages after"}
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
           {unknown: "listener machine \($m | printable) is bound to its durable \(.name | printable) on the bus, and no listener this reading read serves as that machine: name it with --listener \($m | printable)=<url> if it runs outside the cluster, or, if it runs in the cluster and dials the bus by another address, name that NATS_URLS with --bus-alias"}
         else
           {unknown: "listener machine \($m | printable) has its durable \(.name | printable) on the bus and is not bound to it now (offline, asleep or restarting; last active \(.delivered.last_active // "never" | printable)), and no listener this reading read serves as that machine: what it counted while it ran cannot be read; read again while it runs, naming it with --listener \($m | printable)=<url> if it runs outside the cluster, or its NATS_URLS with --bus-alias if it runs in the cluster and dials the bus by another address"}
         end
     else empty end),
    # A listener container on another bus that serves as the machine of a listener this reading
    # read (a load-test stack's listener given a machine of the bus's, or a --bus-alias that names
    # another bus): the durable listener-<machine> on the bus can be either's, so the census above
    # takes the one read for the one not read, and what the one not read counted is never read.
    (($in.other_bus // [])[]
      | select(.machine | type == "string")
      | . as $o
      | ($o.machine | gsub(" "; "-")) as $m
      | [$listeners[] | select(.unknown | not) | select((.machine | strings | gsub(" "; "-")) == $m) | .id] as $read
      | select(($read | length) > 0)
      | {unknown: ("listener pod \($o.ns)/\($o.pod) container \($o.container), on another bus (NATS_URLS \($o.urls // "unset" | masked_urls)) and not read, serves as machine \($o.machine), as \($read | join(" and ")), which this reading read, does: the durable listener-\($m) on the bus can be either's, so this reading cannot show which one it counted; give --bus-alias only a NATS_URLS that reaches the bus's NATS servers, and give a listener on another bus a machine of its own" | printable)}),
    # A listener container on another bus whose ENVOY_MACHINE_ID is no literal value (set through
    # valueFrom, possibly through envFrom, or not at all): the gate cannot read its machine, so it
    # cannot tell whether that machine is one a listener this reading read serves as, which the rule
    # above holds to; it is UNKNOWN, as anything the gate cannot read is.
    (($in.other_bus // [])[]
      | select(.machine | type != "string")
      | {unknown: ("listener pod \(.ns)/\(.pod) container \(.container), on another bus (NATS_URLS \(.urls // "unset" | masked_urls)) and not read, \(if .machine_from == "valueFrom" then "sets ENVOY_MACHINE_ID through valueFrom" elif .machine_from == "envFrom" then "sets no ENVOY_MACHINE_ID of its own and takes envFrom, which may set it" else "sets no ENVOY_MACHINE_ID" end), so its machine cannot be read and whether a listener this reading read serves as that machine cannot be told; set ENVOY_MACHINE_ID on it as a literal value" | printable)}),

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
              | ([$old[] | {subject: ., last: $oldlast[.]}]) as $o
              | (if ($old | length) == 0 then {pass: "the stream holds \($subjects | length) subject(s) under \($family), none in an old form"}
                 # A first reading names what it finds and shuts on none of it (below). A second
                 # reading opens only on a stream with no old-form message at all: each old-form
                 # subject shuts it, named apart when its last message, as the Job read it, came
                 # after the first reading's last stream sequence.
                 elif $in.earlier == null or ($in.earlier.stream.last_seq | type) != "number" then {shut: "the stream holds \($old | length) old-form subject(s) under \($family): \($old[:5] | map(printable) | join(", "))\(if ($old | length) > 5 then ", …" else "" end)", subjects: $old}
                 else
                   ($in.earlier.stream.last_seq) as $l
                   | ([$o[] | select(.last.seq and .last.seq > $l)]) as $after
                   | ([$o[] | select((.last.seq and .last.seq > $l) | not)]) as $still
                   | (if ($after | length) > 0 then {shut: "the stream holds \($after | length) old-form subject(s) under \($family) with a message published after the first reading's last stream sequence \($l): \($after[:5] | map("\(.subject | printable) (sequence \(.last.seq) at \(.last.time // "?" | printable))") | join(", "))\(if ($after | length) > 5 then ", …" else "" end)", subjects: [$after[].subject]} else empty end),
                     (if ($still | length) > 0 then {shut: "the stream still holds \($still | length) old-form subject(s) under \($family) at this second reading, none read as published after the first reading's last stream sequence \($l): \($still[:5] | map("\(.subject | printable) (\(if .last.seq then "sequence \(.last.seq) at \(.last.time // "?" | printable)" elif .last.gone then "expired since its list was read" else "its last message not read" end))") | join(", "))\(if ($still | length) > 5 then ", …" else "" end)", subjects: [$still[].subject]} else empty end)
                 end)
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

# A first reading shuts on no old-form name it finds: old-form messages the stream still holds, and
# interest records and consumer filters still in an old form, are what the rename leaves behind
# until every producer and reader has moved. It names each (FOUND), records the stream's last
# sequence, the newest old-form message and every counter, and the second reading, a window
# later, shuts on any old-form message the stream still holds, any counter that moved, and any
# interest record or consumer filter still in an old form. The newest old-form message is the
# newest of those the Job read; the line says how many old-form subjects' last message it did not.
| ((if $unreadlast.skipped > 0 then ["\($unreadlast.skipped) beyond the \($unreadlast.cap // "?" | printable) the Job reads"] else [] end)
   + (if $unreadlast.failed > 0 then ["\($unreadlast.failed) whose read failed"] else [] end)) as $unreadparts
| (if ($unreadparts | length) > 0 then "; the last message of \($unreadlast.skipped + $unreadlast.failed) old-form subject(s) was not read (\($unreadparts | join(", "))), so the newest named is the newest of those read" else "" end) as $unreadnote
| (if $in.earlier == null then
     [$found[] | if .shut then {found: .shut} + del(.shut) else . end]
     + (if $newestold then [{found: "the newest old-form message on the stream is \($newestold.subject | printable), stream sequence \($newestold.seq) at \($newestold.time // "?" | printable); the stream's last sequence is \($lastseq // "?")\($unreadnote)"}]
        elif ($unreadparts | length) > 0 then [{found: "the last message of no old-form subject was read (\($unreadparts | join(", "))), so no newest old-form message is named; the stream's last sequence is \($lastseq // "?")"}]
        else [] end)
   else $found end) as $found

# --- the comparison with the earlier reading -----------------------------------------------------
| (if $in.earlier == null then []
   else
     ($in.earlier) as $e
     | ($in.started_at - $e.finished_at) as $gap
     | [
         if $e.verdict == "shut" then {shut: "the reading named with --since, taken \($e.read_at), shut the gate, so it is no first reading; take a new first reading"}
         elif $e.verdict != "complete" then {unknown: "the first reading, taken \($e.read_at), did not read every surface; take a new first reading"}
         elif ($e.stream.last_seq | type) != "number" then {unknown: "the first reading recorded no last stream sequence, so the old-form messages published after it cannot be told; take a new first reading"}
         else empty end,
         if $gap < $window then {shut: "\($gap | hours) h passed between the first reading's end (\($e.finished_at | todate)) and this one; the stream keeps \($window | hours) h, so read again at or after \(($e.finished_at + $window + log_line_bound) | todate)"} else empty end,
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
| ([$all[] | select(.found) | .found]) as $foundlines
| {
    verdict: $verdict,
    window: $window,
    lines: (
      [if $identity then "IDENTITY: " + $identity else empty end]
      + [$listeners[] | select(.unknown | not) | "READ: \(.id): \(if .counts == {} then "no old-form name counted" else (.counts | to_entries | map("\(.key | printable) \(.value)") | join(", ")) end) since its start \(if .start then (.start | todate) else "(no start-time gauge)" end)"]
      # A listener container on another bus than the listener Deployment's (a load-test stack's,
      # on its own NATS) is no listener of the bus this reading is about; one on this bus that
      # is missed leaves its durable unread, which the census names. The line names its machine,
      # which the census names too, and how to read it when its NATS_URLS is another address of
      # this bus. Each NATS_URLS is printed with any credential in it masked.
      + [($in.bus_aliases // []) as $aliases
        | ($in.other_bus // [])[]
        | "SKIP: listener pod \(.ns)/\(.pod) container \(.container) is on another bus (NATS_URLS \(.urls // "unset" | masked_urls)), not the listener Deployment's \($in.bus // "" | masked_urls)\(if ($aliases | length) > 0 then " or a --bus-alias (\($aliases | join(", ")))" else "" end), and is not read (machine \(.machine // "unset"); if its NATS_URLS reaches the same NATS servers, name it with --bus-alias)" | printable]
      + [$all[] | select(.pass) | "PASS: " + .pass]
      + [$foundlines[] | "FOUND: " + .]
      + [$shut[] | "GATE SHUT: " + .shut]
      + [$unknown[] | "UNKNOWN: " + .unknown]
    ),
    found: ($foundlines | length),
    last_seq: $lastseq,
    # When to read again: the window after this reading's end, and the age a listener's newest
    # counter line may have besides (log_line_bound), so that each listener's own two reads, the
    # first at or before this reading's end, are a window apart by then however old its line.
    again: ($in.finished_at + $window + log_line_bound),
    record: {
      gate: "legacy-subjects-gate",
      version: 3,
      context: $in.context,
      started_at: $in.started_at,
      finished_at: $in.finished_at,
      read_at: ($in.started_at | todate),
      # A first reading that read every surface, and a second that opened, are each a complete
      # reading: the next window's first reading.
      verdict: (if $verdict == "open" or $verdict == "clean" then "complete" else $verdict end),
      compared_with: (if $in.earlier == null then null else $in.earlier.read_at end),
      identity: {said: $said, credential: $credential},
      stream: {max_age_seconds: $maxage, first_ts: $firstts, created: ($stream.value.created // null), id: $streamid,
        last_seq: $lastseq, newest_old: $newestold, old_last_unread: $unreadlast},
      found: $foundlines,
      listeners: [$listeners[] | select(.unknown | not)],
      consumers_old: [$found[] | select(.consumer) | .consumer],
      subjects_old: [$found[] | select(.subjects) | .subjects[]],
      interests_old: [$found[] | select(.record) | .record],
      shut: [$shut[] | .shut],
      unknown: [$unknown[] | .unknown]
    }
  }
