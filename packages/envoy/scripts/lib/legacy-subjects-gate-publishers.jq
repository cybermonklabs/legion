# packages/envoy/scripts/lib/legacy-subjects-gate-publishers.jq
#
# The readers of fact (a3), the publishers, which legacy-subjects-gate.sh's judge includes: which
# users of the hub may publish a notification subject, read from the hub's auth.conf and, behind
# its auth callout, from the auth service's BROKER_NATS_SERVICES, and what the census of listener
# pins (daemon-release-gate.sh --listeners) printed for each pin key. It calls builtins alone and
# includes no module: jq 1.8.2 fails an assertion (src/compile.c expand_call_arglist) when a
# function of an included module calls one its module included. So each reader answers the text
# it read as it read it, and the judge writes it out (forms' printable).

# reaches_notifications says whether a NATS subject pattern of a publish allow list can match some
# notification subject, notifications.<token>...: `>`, and every pattern of two tokens or more
# whose first token is notifications or `*` (notifications.>, notifications.<x>.>, *.>,
# notifications.agent.x). A pattern of one token matches a subject of one token alone.
def reaches_notifications:
  gsub("^\\s+|\\s+$"; "") as $p
  | ($p | split(".")) as $t
  | $p == ">" or (($t | length) >= 2 and ($t[0] == "notifications" or $t[0] == "*"));

# covers_notifications says whether one deny entry matches every notification subject: `>`, `*.>`
# or notifications.>.
def covers_notifications: gsub("^\\s+|\\s+$"; "") | . == ">" or . == "*.>" or . == "notifications.>";

# can_publish_notifications says whether a user with these permissions (an auth.conf user's
# permissions object as rendered_user holds it) may publish some notification subject. A publish
# direction that is missing, or whose allow list is missing, empty or holds a blank entry, allows
# every subject: nats-server applies no allow list to that direction (natsgrant.OpenDirection).
# Otherwise one entry must reach a notification subject. A deny list takes that away only through
# one entry that matches every notification subject; any other deny list is read as leaving one
# open, which can only keep the gate shut.
def can_publish_notifications:
  (.publish.allow // []) as $allow
  | (.publish.deny // []) as $deny
  | (($allow | length) == 0 or any($allow[]; gsub("\\s"; "") == "")) as $open
  | ($open or any($allow[]; reaches_notifications)) and (any($deny[]; covers_notifications) | not);

# pyjson is the text Python's json.dumps writes for a value with sort_keys and its default
# separators (", " and ": "), non-ASCII raw: infra/core's PyJSON, as Authorization.Render writes
# each user's object. A string is tojson's, which is Python's for every string Render's Validate
# lets into a subject (no control character, no DEL).
def pyjson:
  if type == "object" then "{" + (to_entries | sort_by(.key) | map((.key | tojson) + ": " + (.value | pyjson)) | join(", ")) + "}"
  elif type == "array" then "[" + (map(pyjson) | join(", ")) + "]"
  else tojson end;

# rendered_user says whether a user's object has the keys Render writes, spelled as it spells them:
# nkey, a user's key, and permissions, whose keys are publish and subscribe, each an object whose
# keys are allow and deny, each a list of strings. nats-server reads other spellings as these too
# (pub for publish, permission for permissions, in any case: v2.15.1 server/opts.go), and keeps
# whichever of two it reads last, so an object holding any other key could publish what this reader
# does not see.
def rendered_user:
  type == "object" and keys == ["nkey", "permissions"]
  and (.nkey | type == "string" and test("^U[A-Z0-9]{55}$"))
  and (.permissions | type == "object" and all(keys[]; . == "publish" or . == "subscribe")
    and all(.[]; type == "object" and all(keys[]; . == "allow" or . == "deny")
      and all(.[]; type == "array" and all(.[]; type == "string"))));

# auth_conf reads the hub's auth.conf as infra/core's Authorization.Render writes it, each line
# whole: its comment header, `authorization {`, `  users: [`, each user as a `    # <name>` line and
# then its one JSON object, `  ]`, the auth callout's block when the hub runs one (issuer,
# auth_users and xkey, in that order), `}`, and after it comment lines, no_auth_user and the
# callout's ping_interval and ping_max. A line is read only as a whole: nats-server ends a value at
# `;` or `,` and reads on (v2.15.1 conf/lex.go), so a line that only begins as one of these could
# carry more, an accounts block among it. A user's object must have rendered_user's keys and be
# the text pyjson writes for what it holds, so a key written twice or escaped, which this reader
# and nats-server could read apart, is no user's object. It answers {users: [{name, nkey,
# permissions}], callout} or {error: {line, text, why}} for a line of any other shape, and {error:
# {why, names}} for two users of one name or a text that ends inside the block, since a user this
# reader passed over could be a publisher it never saw.
def auth_conf:
  "U[A-Z0-9]{55}" as $user_key
  | ["^    issuer: \"[A-Z0-9]{56}\"$", "^    auth_users: \\[(\"\($user_key)\"(, \"\($user_key)\")*)?\\]$", "^    xkey: \"[A-Z0-9]{56}\"$"] as $callout_lines
  | "^no_auth_user: \"\($user_key)\"$|^ping_interval: \"[0-9]+[a-z]+\"$|^ping_max: [0-9]+$" as $tail_line
  | split("\n") as $lines
  | reduce range(0; $lines | length) as $i ({state: "header", users: [], name: null, callout: false, error: null};
      if .error != null then .
      else
        $lines[$i] as $l
        | {line: ($i + 1), text: $l} as $at
        | if .state == "header" then
            if $l == "authorization {" then .state = "block"
            elif $l == "" or ($l | startswith("#")) then .
            else .error = $at + {why: "comes before its authorization block"} end
          elif .state == "block" then
            if $l == "  users: [" then .state = "users" else .error = $at + {why: "is not its users list"} end
          elif .state == "users" then
            if .name == null and ($l | startswith("    # ")) then .name = ($l | ltrimstr("    # "))
            elif .name != null and ($l | startswith("    {")) then
              ($l | ltrimstr("    ") | try fromjson catch null) as $u
              | if ($u | rendered_user) and "    \($u | pyjson)" == $l then
                  .users += [{name: .name, nkey: $u.nkey, permissions: $u.permissions}] | .name = null
                else .error = $at + {why: "is no user's JSON object as the rendering writes it: the keys nkey and permissions, publish and subscribe, allow and deny, each once, sorted and unescaped"} end
            elif .name == null and $l == "  ]" then .state = "users_end"
            else .error = $at + {why: "is no user's name or object of its users list"} end
          elif .state == "users_end" then
            if $l == "  auth_callout {" then .state = "callout" | .callout = true | .callout_line = 0
            elif $l == "}" then .state = "tail"
            else .error = $at + {why: "follows its users list"} end
          elif .state == "callout" then
            .callout_line as $n
            | if $n < ($callout_lines | length) and ($l | test($callout_lines[$n])) then .callout_line += 1
              elif $n == ($callout_lines | length) and $l == "  }" then .state = "callout_end"
              else .error = $at + {why: "is no line of its auth_callout block"} end
          elif .state == "callout_end" then
            if $l == "}" then .state = "tail" else .error = $at + {why: "follows its auth_callout block"} end
          else
            if $l == "" or ($l | startswith("#")) or ($l | test($tail_line)) then .
            else .error = $at + {why: "follows its authorization block"} end
          end
      end)
  | if .error != null then {error: .error}
    elif .state != "tail" then {error: {why: "it ends inside its authorization block"}}
    elif (.users | map(.name) | length) != (.users | map(.name) | unique | length) then
      {error: {why: "two of its users share a name:", names: (.users | group_by(.name) | map(select(length > 1) | .[0].name))}}
    else {users: .users, callout: .callout} end;

# nats_services reads BROKER_NATS_SERVICES as the broker's config.ParseNATSServices does, for the
# part fact (a3) needs: whitespace-separated <kind>/<machine id or ->/<scope>=<account> entries,
# each answered with its kind and the name the auth service's answer gives its user, <kind>/<machine
# id>, or <kind>/<account> for an entry of no machine id (config.NATSService Name). An entry of
# any other shape is an error, {error: {entry}}, since its kind could be a publisher's.
def nats_services:
  [splits("\\s+") | select(length > 0)] as $entries
  | "^(?<kind>[a-z][a-z-]*)/(?<machine>[^/=\\s]+)/(?<scope>[^/=\\s]+)=(?<account>\\S+)$" as $form
  | ([$entries[] | select(test($form) | not)] | first) as $bad
  | if $bad != null then {error: {entry: $bad}}
    else {entries: [$entries[] | capture($form) | {kind, identity: "\(.kind)/\(if .machine == "-" then .account else .machine end)"}]} end;

# listener_verdict($lines; $key) is what the census of listener pins printed for $key, its lines as
# daemon-release-gate.sh --listeners prints them: shut on a `GATE SHUT: <key> ` line, unknown on an
# `UNKNOWN: <key>: ` line, pass on a `PASS: <key> runs ` line, and unknown when it printed none.
def listener_verdict($lines; $key):
  ([$lines[] | select(startswith("GATE SHUT: \($key) "))] | first) as $shut
  | ([$lines[] | select(startswith("UNKNOWN: \($key): "))] | first) as $unknown
  | ([$lines[] | select(startswith("PASS: \($key) runs "))] | first) as $pass
  | if $shut then {verdict: "shut", line: $shut}
    elif $unknown then {verdict: "unknown", line: $unknown}
    elif $pass then {verdict: "pass", line: $pass}
    else {verdict: "unknown", line: "the census of listener pins printed no line for \($key)"} end;
