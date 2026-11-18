# shellcheck shell=bash
# The seeded corpus of dispatch://INFRA-1150, which its four drivers that read rows an earlier
# release wrote share: answer-attribute-removal.sh, answered-decisions.sh, restore-redirects.sh
# and decision-nodes.sh. Every row of it is written by the base release, the server lib-baseline.sh
# boots (BASELINE_BINARY, built and stamped from BASELINE_SHA), through that server's own HTTP
# API: an ask is an `:::ask` block of a document upload or an issue's spec, which that server
# settles into its row and a person then answers through its answer route. Nothing here writes a
# row through the database or through this branch's code, so what a driver reads afterwards is the
# state the release before this branch left. Sourced, never run, after lib.sh and lib-baseline.sh.
#
# A person acts through the dev sign-in (`/auth/_dev/signin?login=alice`, its cookie kept in a
# jar, every write naming the server's origin); an agent through the bearer baseline_start leaves
# in baseline_token, naming its session in `actor`. The bearer goes into a header file under the
# run's $work and is printed nowhere.
#
# The functions it gives the caller:
#
#   seed_reset_database URL
#       Drops the database URL names and creates it empty again, so that the base release's own
#       Store.Migrate writes its schema and the corpus is the database's only content. Each driver
#       calls it before baseline_start, since a second run over a database the first one seeded
#       would count both corpora. It drops only a database on this machine (127.0.0.1, localhost or
#       ::1) whose name begins `infra_1150_`, the databases these drivers alone use, and refuses
#       every other with status 2: never the shared legion_test, never a deployment's. It connects
#       to the `postgres` database of the same server, with the same role, to drop and create.
#
#   seed_decisions
#       Writes the corpus. It reads baseline_url, baseline_token and baseline_database_url, which
#       baseline_start set, and the caller's `work`, which lib.sh's work_dir set: its files go under
#       $work/seed. It returns once every document of the corpus is settled (no row of
#       doc_settlements_pending, the one read of the database it makes, which no route answers) and
#       each approval it wrote still stands at its document's newest version. A write the server
#       refuses, and a wait that runs out, print `FAIL: seed <what>: <observed>` and exit 1, as
#       lib.sh's fail does.
#
#       It prints every id it wrote, one `key=value` line each - and the words of each answer and
#       of the decline's note (answer_<ask>, decline_note), the version each review names
#       (<review>_version), and the version that superseded spec D's approval
#       (review_spec_d_superseded_by_version) - and keeps each in the associative array seed_ids
#       under the same key, which a driver reads after the call. Then it prints the corpus's own
#       counts for the measurements of the spec's section "What this child measures before it
#       writes a migration", which a driver compares with what its queries read:
#           corpus_documents_holding_an_ask=<n>
#               the documents it wrote an ask block into, open or answered or declined
#           corpus_standing_approvals_over_those_documents=<n>
#               of those, the ones whose newest review is a person's approval of the newest version
#           corpus_colliding_block_ids=<n>, and corpus_collision_<n>=<KEY> <block id> <shape>
#               each ask whose block id a live block of its issue's spec holds that is not its block
#
# The corpus, in project SEED, whose gated steps hold `decisions` and `spec`:
#
#   Issue A (issue_a), the main seeded issue. Its spec (spec_a) holds four requirement blocks
#   (r-store, r-format, r-backup, r-reader) and five asks a person settled: ask_kept (#q-kept),
#   answered with an option and words of the person's own, whose block stays; ask_open (#q-open),
#   still open; ask_declined (#q-declined), declined with a note; and two answered asks whose
#   blocks an agent then removed, so that each row stays `answered` naming the spec, the main
#   shape of the restore: ask_left_store (#q-left-store) and ask_left_format (#q-left-format).
#   A callout of the spec now holds the id q-left-store, the collision of the main shape, and a
#   second callout holds q-doc-collide, the block id of the decisions document's second ask, the
#   collision of the second shape. The spec's `## Decisions` part holds one pointer line, to
#   dispatch://<A>/artifact/decisions-md. A person approved the spec last, at its newest version,
#   while it still held the answered ask_kept (review_spec_a).
#
#   Its decisions.md (decisions_a), of the old shape, has seven sections:
#     - "Readings come from the seeded database" restates ask_left_store's question and answer in
#       other words and adds words of its own, links three spec blocks (r-store, r-backup and
#       r-reader) and cites dispatch://<A>/ask/<ask_left_store> (section_left_store);
#     - a section whose heading cites dispatch://<A>/ask/<ask_left_format> and whose body is that
#       ask's answer and nothing more, in other case and spacing (section_left_format);
#     - "Files stay out of the corpus", which cites no ask, links r-format and holds an `Unsure:`
#       paragraph (section_unsure);
#     - "The census runs before the view", which cites no ask and carries a standing Keep a
#       person recorded through the Keep route (section_kept);
#     - "Which reader reads the corpus first", holding ask block #q-doc-plain (ask_doc_plain), an
#       answered ask whose row names the decisions document, the second shape of the restore;
#     - "Which line names a moved block", holding ask block #q-doc-collide (ask_doc_collide),
#       answered, whose block id the spec holds too;
#     - `## Replaced`, holding one `###` section (section_replaced).
#   A third document, notes.md (notes_a), holds one block citing
#   dispatch://<A>/artifact/decisions-md#b-q-doc-plain, which a comment (comment_decisions_block)
#   cites too; a second comment (comment_old_spec_address) cites the old spec address of
#   ask_left_format, dispatch://<A>/spec#b-q-left-format, which no block holds. A person approved
#   notes.md (review_notes_a), a document that holds no ask block.
#
#   Issue B (issue_b): its spec (spec_b) has no `## Decisions` part. It held ask_b_left
#   (#q-b-left), which a person answered and an agent then removed, and a person approved the
#   spec after the ask left (review_spec_b). Its decisions.md (decisions_b) restates that answer
#   in one section, cites the ask and links r-b-one.
#
#   Issue C (issue_c): its spec (spec_c) is a document whose only ask (ask_c_open, #q-c-open) is
#   open, and a person asked for changes on it (review_spec_c), so its newest review is no
#   approval.
#
#   Issue D (issue_d): a person approved its spec (spec_d) at its newest version while it held
#   ask_d_approved (#q-d-approved), which the person had answered (review_spec_d); an agent then
#   removed that block, so the row stays `answered` naming the spec, and the newer version
#   (review_spec_d_superseded_by_version) leaves that approval standing at no newest version.
#
# So the corpus holds five documents with an ask block (spec_a, decisions_a, spec_b, spec_c,
# spec_d), two of them under a standing approval (spec_a and spec_b), three reviews the second
# count must leave out (an approval of notes.md, which holds no ask block; spec_c's request for
# changes; and spec_d's approval, which a newer version superseded), and two collisions, one of
# each shape.

# What the functions below read of their caller, named here so that a reader of this file sees
# it, as does shellcheck, which reads one file at a time: lib.sh's work_dir sets `work`, and
# lib-baseline.sh's baseline_start the three baseline_ values. Each resolves when a function runs.
: "${work:=}" "${baseline_url:=}" "${baseline_token:=}" "${baseline_database_url:=}"

# What seed_decisions leaves its caller, by the keys its key=value lines print.
declare -gA seed_ids=()

# The person every write of a person is made as, and the session every agent write names.
readonly seed_person=alice
readonly seed_session=seed-decisions-agent
readonly seed_project=SEED

# How long one wait of the seed may take, in tenths of a second: a settlement the server runs on
# a timer after a write lands within a few seconds.
readonly seed_wait_limit=600

# What seed_decisions wrote, by artifact id: every document it wrote an ask block into, and every
# document whose last review it wrote is a person's approval, with that review's version.
declare -gA seed_ask_documents=()
declare -gA seed_approved_version=()
# The collisions it wrote, one "<KEY> <block id> <shape>" each.
seed_collisions=()

# seed_refuse WORDS…: an input or a call this file will not take. It exits 2, as lib.sh's
# require_env does.
seed_refuse() {
  printf 'seed-decisions.sh: %s\n' "$*" >&2
  exit 2
}

# seed_reset_database URL: the head comment above is its contract.
seed_reset_database() {
  local url="$1" scheme authority name query host
  [[ -n "$work" ]] || seed_refuse "seed_reset_database writes its log under \$work: call lib.sh's work_dir first"
  [[ "$url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] ||
    seed_refuse "the database URL is no postgres:// URL naming a database"
  scheme="${BASH_REMATCH[1]}"
  authority="${BASH_REMATCH[3]}"
  name="${BASH_REMATCH[4]}"
  query="${BASH_REMATCH[5]}"
  host="${authority##*@}"
  if [[ "$host" == \[* ]]; then
    host="${host%%]*}]"
  else
    host="${host%%:*}"
  fi
  case "$host" in
    127.0.0.1 | localhost | '[::1]') ;;
    *) seed_refuse "the database is on $host, not this machine: this seed drops and creates only this pod's own test databases" ;;
  esac
  [[ "$name" =~ ^infra_1150_[a-z0-9_]+$ ]] ||
    seed_refuse "the database is $name: this seed drops and creates only a database whose name begins infra_1150_, one of this child's drivers' own"
  baseline_psql "${scheme}${authority}/postgres${query}" -q \
    -c "drop database if exists \"$name\" with (force)" -c "create database \"$name\"" \
    >"$work/seed-reset" 2>&1 || fail seed "drop and create the database $name" "psql: $(<"$work/seed-reset")"
  printf 'seed database %s dropped and created empty\n' "$name"
}

# seed_record KEY VALUE: one id the corpus holds, printed and kept in seed_ids.
seed_record() {
  seed_ids["$1"]="$2"
  printf '%s=%s\n' "$1" "$2"
}

# seed_request AS METHOD PATH [JSON]: sends METHOD PATH to the base release as AS, `agent` (the
# bearer) or `person` (the dev sign-in's cookie and the server's origin, which a cookie's write
# must name). JSON is a body, or @FILE for one held in a file. The answer's body goes to
# $seed_dir/body and its HTTP status to seed_status.
seed_request() {
  local -a auth data=()
  case "$1" in
    person) auth=(-b "$seed_dir/jar" -H "Origin: $baseline_url") ;;
    agent) auth=(-H "@$seed_dir/auth") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  seed_status="$(curl -sS -X "$2" -o "$seed_dir/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$baseline_url$3" 2>"$seed_dir/curl")" || seed_status="curl failed: $(<"$seed_dir/curl")"
}

# seed_want CODE WHAT: the last answer's status is CODE.
seed_want() {
  [[ "$seed_status" == "$1" ]] ||
    fail seed "$2" "HTTP $seed_status, want $1: $(head -c 600 "$seed_dir/body")"
}

# seed_field WHAT JQ: one value of the last answer's body, which must be there, in seed_value. It
# sets a variable rather than printing the value, and its caller reads seed_value afterwards: a
# fail inside `$(…)` would end only that subshell, and the seed would go on with an empty value.
seed_field() {
  seed_value="$(jq -er "$2" "$seed_dir/body" 2>/dev/null)" ||
    fail seed "$1" "no $2 in: $(head -c 600 "$seed_dir/body")"
}

# seed_actor: the actor member every agent write names.
seed_actor() { jq -cn --arg s "$seed_session" '{kind: "session", id: $s}'; }

# seed_wait_settled WHAT: waits until no document owes a settlement. An answer and a decline write
# the live document and leave its next version to the settlement a timer runs, so a review written
# before that version lands would approve a version that is no longer the newest.
seed_wait_settled() {
  local waited=0 owed
  while true; do
    owed="$(baseline_psql "$baseline_database_url" -Atqc \
      'select count(*) from doc_settlements_pending' 2>"$seed_dir/psql")" ||
      fail seed "$1" "read doc_settlements_pending: $(<"$seed_dir/psql")"
    [[ "$owed" != 0 ]] || return 0
    [[ "$waited" -lt "$seed_wait_limit" ]] ||
      fail seed "$1" "$owed documents still owe a settlement after $((seed_wait_limit / 10)) s"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# seed_wait_block DOCUMENT BLOCK STATE WHAT: waits until DOCUMENT's newest version writes the ask
# block BLOCK with state="STATE", which is the version the answer or the decline made.
seed_wait_block() {
  local waited=0
  while true; do
    seed_request agent GET "/api/v1/artifacts/$1/text"
    seed_want 200 "$4"
    jq -e --arg open ":::ask{#$2 " --arg state "state=\"$3\"" \
      '[.markdown | split("\n")[] | select(startswith($open)) | contains($state)] == [true]' \
      "$seed_dir/body" >/dev/null && return 0
    [[ "$waited" -lt "$seed_wait_limit" ]] ||
      fail seed "$4" "block $2 does not read state=\"$3\" after $((seed_wait_limit / 10)) s: $(head -c 600 "$seed_dir/body")"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# seed_issue KEY_NAME SPEC_NAME TITLE FILE: creates an issue of the project whose spec is FILE's
# markdown, and records its key and its spec's id.
seed_issue() {
  jq -cn --arg p "$seed_project" --arg t "$3" --rawfile s "$4" --argjson a "$(seed_actor)" \
    '{project: $p, title: $t, spec: $s, actor: $a}' >"$seed_dir/issue.json"
  seed_request agent POST /api/v1/issues "@$seed_dir/issue.json"
  seed_want 201 "create the issue \"$3\""
  seed_field "create the issue \"$3\"" .key
  seed_record "$1" "$seed_value"
  seed_field "create the issue \"$3\"" .primary_artifact_id
  seed_record "$2" "$seed_value"
}

# seed_upload KEY_NAME ISSUE NAME FILE: uploads FILE as the document NAME of ISSUE, as an agent,
# and records its id.
seed_upload() {
  jq -cn --arg n "$3" --rawfile c "$4" --argjson a "$(seed_actor)" \
    '{name: $n, content: $c, actor: $a}' >"$seed_dir/upload.json"
  seed_request agent POST "/api/v1/issues/$2/artifacts" "@$seed_dir/upload.json"
  seed_want 201 "upload $3 to $2"
  seed_field "upload $3 to $2" .artifact.id
  seed_record "$1" "$seed_value"
}

# seed_edit DOCUMENT WHAT OPS: applies the edit ops OPS (a JSON list) to DOCUMENT as an agent.
seed_edit() {
  seed_request agent POST "/api/v1/artifacts/$1/edits" \
    "$(jq -cn --argjson ops "$3" --argjson a "$(seed_actor)" '{ops: $ops, actor: $a}')"
  seed_want 200 "$2"
  jq -e '.changed == true' "$seed_dir/body" >/dev/null ||
    fail seed "$2" "the edit changed nothing: $(head -c 600 "$seed_dir/body")"
}

# seed_ask KEY_NAME ISSUE DOCUMENT BLOCK: records the ask the base release settled for the ask
# block BLOCK of DOCUMENT, waiting for its row, and marks DOCUMENT as one holding an ask block.
seed_ask() {
  local waited=0 id=""
  while true; do
    seed_request agent GET "/api/v1/issues/$2/asks"
    seed_want 200 "read the asks of $2"
    id="$(jq -r --arg b "$4" --arg d "$3" \
      '[.[] | select(.block_id == $b and .block_artifact.id == $d) | .id] | if length == 1 then .[0] else "" end' \
      "$seed_dir/body")" || id=""
    [[ -z "$id" ]] || break
    [[ "$waited" -lt "$seed_wait_limit" ]] ||
      fail seed "the ask of block $4" "no ask of $2 names block $4 of $3 after $((seed_wait_limit / 10)) s"
    sleep 0.1
    waited=$((waited + 1))
  done
  seed_ask_documents["$3"]=1
  seed_record "$1" "$id"
}

# seed_answer ASK_KEY DOCUMENT BLOCK TEXT [OPTION]: the person answers the ask seed_ids[ASK_KEY]
# names in the words TEXT, selecting OPTION where it is given, and the answer's version of
# DOCUMENT lands. The words are recorded as answer_<ASK_KEY without its ask_>.
seed_answer() {
  seed_request person POST "/api/v1/asks/${seed_ids[$1]}/answer" \
    "$(jq -cn --arg t "$4" --arg o "${5:-}" \
      '{text: $t, selected: (if $o == "" then [] else [$o] end), expected_edited_at: null}')"
  seed_want 200 "$seed_person answers the ask of block $3"
  seed_wait_block "$2" "$3" answered "the answer to block $3 lands in its document"
  seed_record "answer_${1#ask_}" "$4"
}

# seed_review KEY_NAME DOCUMENT STATE [REASON]: the person reviews DOCUMENT at its newest version,
# and the review is recorded with the version it names.
seed_review() {
  seed_request person POST "/api/v1/artifacts/$2/reviews" \
    "$(jq -cn --arg s "$3" --arg r "${4:-}" '{state: $s} + (if $r == "" then {} else {reason: $r} end)')"
  seed_want 201 "$seed_person reviews $2 ($3)"
  seed_field "$seed_person reviews $2" .id
  seed_record "$1" "$seed_value"
  seed_field "$seed_person reviews $2" .version
  seed_record "${1}_version" "$seed_value"
  if [[ "$3" == approved ]]; then
    seed_approved_version["$2"]="${seed_ids[${1}_version]}"
  else
    unset 'seed_approved_version[$2]'
  fi
}

# seed_decision_block WHAT ISSUE HEADING: the heading block id and the text sum of the decision
# HEADING of ISSUE's decisions.md, as the base release's Decisions read answers them, in
# seed_decision_id and seed_decision_sum.
seed_decision_block() {
  seed_request agent GET "/api/v1/issues/$2/decisions"
  seed_want 200 "$1"
  seed_decision_id="$(jq -er --arg h "$3" \
    '[.decisions[] | select(.heading == $h)] | if length == 1 then .[0].block_id else error end' \
    "$seed_dir/body" 2>/dev/null)" || fail seed "$1" "no one decision \"$3\" in: $(head -c 600 "$seed_dir/body")"
  seed_decision_sum="$(jq -er --arg h "$3" '.decisions[] | select(.heading == $h) | .text_sha256' \
    "$seed_dir/body")" || fail seed "$1" "no text_sha256 for \"$3\""
}

# seed_decisions: the head comment above is its contract.
seed_decisions() {
  [[ -n "$baseline_url" && -n "$baseline_token" && -n "$baseline_database_url" ]] ||
    seed_refuse "seed_decisions writes through the server baseline_start boots: call baseline_start first"
  [[ -n "$work" ]] || seed_refuse "seed_decisions writes its files under \$work: call lib.sh's work_dir first"
  require_commands curl grep head jq psql sleep
  seed_dir="$work/seed"
  mkdir -p "$seed_dir"
  printf 'Authorization: Bearer %s\n' "$baseline_token" >"$seed_dir/auth"

  # The person, signed in through the dev sign-in. The route answers a redirect that sets the
  # session cookie. curl writes its jar's header lines whether or not a cookie came, so the jar is
  # read for a cookie line, which alone holds a tab (an HttpOnly one opens `#HttpOnly_`).
  seed_status="$(curl -sS -o "$seed_dir/body" -w '%{http_code}' -c "$seed_dir/jar" \
    "$baseline_url/auth/_dev/signin?login=$seed_person" 2>"$seed_dir/curl")" ||
    fail seed "sign $seed_person in" "curl: $(<"$seed_dir/curl")"
  [[ "$seed_status" =~ ^[23][0-9][0-9]$ ]] ||
    fail seed "sign $seed_person in" "HTTP $seed_status: $(head -c 600 "$seed_dir/body")"
  grep -q $'\t' "$seed_dir/jar" ||
    fail seed "sign $seed_person in" "the dev sign-in route answered HTTP $seed_status and set no cookie"
  seed_record person "$seed_person"

  # The project, whose gated steps hold `decisions`.
  seed_request agent POST /api/v1/projects "$(jq -cn --arg k "$seed_project" --arg p "$seed_person" \
    --argjson a "$(seed_actor)" '{key: $k, name: "Seeded decisions corpus", default_assignee: $p, actor: $a}')"
  seed_want 201 "create the project $seed_project"
  seed_record project "$seed_project"
  seed_request agent PUT "/api/v1/projects/$seed_project/gated-steps" \
    "$(jq -cn --argjson a "$(seed_actor)" '{steps: ["decisions", "spec"], actor: $a}')"
  seed_want 200 "publish the gated steps of $seed_project"
  jq -e '.steps == ["decisions", "spec"]' "$seed_dir/body" >/dev/null ||
    fail seed "publish the gated steps of $seed_project" "$(head -c 600 "$seed_dir/body")"
  seed_record project_gated_steps decisions,spec

  # Issue A, its spec holding five asks.
  cat >"$seed_dir/spec-a.md" <<'MD'
# Seeded corpus

The corpus the measurements of this child read.

## Requirements

:::requirement{#r-store}
The corpus is written by the release before this branch, through its own API.
:::

:::requirement{#r-format}
Each measurement is one line of the pull request body, with the time it was taken.
:::

:::requirement{#r-backup}
No reading needs a credential for the deployment.
:::

:::requirement{#r-reader}
A reader can tell which database a reading came from.
:::

## Questions

:::ask{#q-kept urgency="med"}
Which store holds the seeded corpus?

- Postgres: the database the pod already runs.
- Files: a directory beside the server.
:::

:::ask{#q-open urgency="low"}
Should the census also print how large the corpus is?
:::

:::ask{#q-declined urgency="low"}
Which colour should the census print its counts in?

- Green: easy to spot.
- Plain: what a log holds.
:::

:::ask{#q-left-store urgency="high"}
Where do the measurements read their rows from?

- The pod's test database: seeded through the base release.
- A copy of production: which no pod can reach.
:::

:::ask{#q-left-format urgency="med"}
How is each measurement written into the body?
:::
MD
  seed_issue issue_a spec_a "Seeded corpus: the main issue" "$seed_dir/spec-a.md"
  local a="${seed_ids[issue_a]}" spec_a="${seed_ids[spec_a]}"
  seed_ask ask_kept "$a" "$spec_a" q-kept
  seed_ask ask_open "$a" "$spec_a" q-open
  seed_ask ask_declined "$a" "$spec_a" q-declined
  seed_ask ask_left_store "$a" "$spec_a" q-left-store
  seed_ask ask_left_format "$a" "$spec_a" q-left-format

  # Issue B, whose spec has no `## Decisions` part.
  cat >"$seed_dir/spec-b.md" <<'MD'
# Second seeded spec

:::requirement{#r-b-one}
The corpus is written once a run, by one build.
:::

:::ask{#q-b-left urgency="med"}
Which build writes the corpus?
:::
MD
  seed_issue issue_b spec_b "Seeded corpus: a spec with no Decisions part" "$seed_dir/spec-b.md"
  local b="${seed_ids[issue_b]}" spec_b="${seed_ids[spec_b]}"
  seed_ask ask_b_left "$b" "$spec_b" q-b-left

  # Issue C, whose spec's only ask stays open.
  cat >"$seed_dir/spec-c.md" <<'MD'
# Third seeded spec

A document whose only ask is open.

:::ask{#q-c-open urgency="low"}
Does the corpus need a fourth issue?
:::
MD
  seed_issue issue_c spec_c "Seeded corpus: a document whose only ask is open" "$seed_dir/spec-c.md"
  seed_ask ask_c_open "${seed_ids[issue_c]}" "${seed_ids[spec_c]}" q-c-open

  # Issue D, whose spec a person approves while it holds its answered ask, before the ask leaves.
  cat >"$seed_dir/spec-d.md" <<'MD'
# Fourth seeded spec

:::requirement{#r-d-one}
An approval names the version the person read.
:::

:::ask{#q-d-approved urgency="med"}
Which version does an approval name?
:::
MD
  seed_issue issue_d spec_d "Seeded corpus: a spec approved before its answered ask left" "$seed_dir/spec-d.md"
  local spec_d="${seed_ids[spec_d]}"
  seed_ask ask_d_approved "${seed_ids[issue_d]}" "$spec_d" q-d-approved

  # The person settles the asks: three answers on spec A, one on spec B, one on spec D, and a
  # decline.
  seed_answer ask_kept "$spec_a" q-kept \
    "Postgres, since it is the store we already back up every night." Postgres
  seed_answer ask_left_store "$spec_a" q-left-store \
    "Read the seeded test database; no pod of this tree can reach production."
  seed_answer ask_left_format "$spec_a" q-left-format \
    "One line for each measurement, with the time it was taken."
  seed_answer ask_b_left "$spec_b" q-b-left "The base release writes it, through its own API."
  seed_answer ask_d_approved "$spec_d" q-d-approved "The version the person read when they approved."
  seed_request person POST "/api/v1/asks/${seed_ids[ask_declined]}/decline" \
    '{"note":"Colour is not mine to choose here; print plain text."}'
  seed_want 200 "$seed_person declines the ask of block q-declined"
  seed_wait_block "$spec_a" q-declined resolved "the decline of block q-declined lands in the spec"
  seed_record decline_note "Colour is not mine to choose here; print plain text."
  seed_wait_settled "the answers settle"

  # The person approves spec D at its newest version, which holds the answered ask_d_approved.
  seed_review review_spec_d "$spec_d" approved

  # An agent removes the two answered blocks from spec A, and the one from spec B and from spec D:
  # each row stays answered, naming its spec. Two callouts then take the ids q-left-store (the
  # main shape's collision) and q-doc-collide (the id the decisions document's second ask is
  # written under).
  seed_edit "$spec_a" "an agent removes blocks q-left-store and q-left-format from spec A" \
    '[{"op":"delete","block":"q-left-store"},{"op":"delete","block":"q-left-format"}]'
  seed_edit "$spec_a" "callouts of spec A take the ids q-left-store and q-doc-collide" \
    "$(jq -cn '[{op: "insert", after: "block:r-reader", markdown: (":::callout{#q-left-store kind=\"note\" title=\"\"}\nThe readings name the database they came from.\n:::\n\n:::callout{#q-doc-collide kind=\"note\" title=\"\"}\nA moved block keeps a line of the census.\n:::")}]')"
  seed_collisions+=("$a q-left-store block_left_spec")
  seed_edit "$spec_b" "an agent removes block q-b-left from spec B" '[{"op":"delete","block":"q-b-left"}]'
  seed_edit "$spec_d" "an agent removes block q-d-approved from spec D" \
    '[{"op":"delete","block":"q-d-approved"}]'
  # That version is newer than the one the person approved, so the approval no longer stands: it
  # names a version that held the answered ask, and the newest holds none.
  seed_request agent GET "/api/v1/artifacts/$spec_d/text"
  seed_want 200 "read spec D's newest version"
  seed_field "read spec D's newest version" .version
  [[ "$seed_value" -gt "${seed_ids[review_spec_d_version]}" ]] ||
    fail seed "the removal of q-d-approved supersedes the approval of spec D" \
      "the approval names version ${seed_ids[review_spec_d_version]}, and the newest is $seed_value"
  seed_record review_spec_d_superseded_by_version "$seed_value"
  unset 'seed_approved_version[$spec_d]'

  # Spec A's decisions.md, of the old shape: the main shape's two restatements, two sections that
  # cite no ask, two answered ask blocks of its own (the second shape), and `## Replaced`.
  cat >"$seed_dir/decisions-a.md" <<MD
# Decisions

## Readings come from the seeded database

The person ruled that every reading is taken over the pod's own seeded database and never over production, which no pod of this tree can reach. We add that the corpus stays small enough to seed in under a minute. This shapes dispatch://$a/spec#b-r-store, dispatch://$a/spec#b-r-backup and dispatch://$a/spec#b-r-reader. Decided in dispatch://$a/ask/${seed_ids[ask_left_store]}.

## How a reading is written: dispatch://$a/ask/${seed_ids[ask_left_format]}

one line for each measurement,  with the time it was taken.

## Files stay out of the corpus

We write no file beside the database, which dispatch://$a/spec#b-r-format depends on.

Unsure: a reader who wants a file export would change this.

## The census runs before the view

The census reads the corpus before any view of it is drawn.

## Which reader reads the corpus first

:::ask{#q-doc-plain urgency="med"}
Which reader reads the corpus first?

- The census: it counts.
- The decisions view: it shows.
:::

## Which line names a moved block

:::ask{#q-doc-collide urgency="low"}
Which line names a block that moved?
:::

## Replaced

### Files held the corpus once

We first kept the corpus in files, and replaced that with the database.
MD
  seed_upload decisions_a "$a" decisions.md "$seed_dir/decisions-a.md"
  local decisions_a="${seed_ids[decisions_a]}"
  seed_ask ask_doc_plain "$a" "$decisions_a" q-doc-plain
  seed_ask ask_doc_collide "$a" "$decisions_a" q-doc-collide
  seed_collisions+=("$a q-doc-collide block_in_decisions_document")
  seed_answer ask_doc_plain "$decisions_a" q-doc-plain "The census reads it first." "The census"
  seed_answer ask_doc_collide "$decisions_a" q-doc-collide \
    "The census names each move on a line of its own."

  # Spec A's `## Decisions` part, its one pointer line.
  seed_edit "$spec_a" "spec A's Decisions part holds one pointer line" \
    "$(jq -cn --arg d "dispatch://$a/artifact/decisions-md" \
      '[{op: "insert", after: "end", markdown: ("## Decisions\n\nThe decisions this spec rests on are in " + $d + ".")}]')"

  # Spec B's decisions.md: one section restating its answer.
  cat >"$seed_dir/decisions-b.md" <<MD
# Decisions

## The release before this one writes the corpus

The person chose the older build, writing through the routes it serves itself, so every row is that release's. This shapes dispatch://$b/spec#b-r-b-one. Decided in dispatch://$b/ask/${seed_ids[ask_b_left]}.
MD
  seed_upload decisions_b "$b" decisions.md "$seed_dir/decisions-b.md"

  # The citations of the second shape's ask block in the decisions document: a block of a third
  # document and a comment. A second comment cites an old spec address of the main shape.
  cat >"$seed_dir/notes-a.md" <<MD
# Notes

The census reads the corpus first, as dispatch://$a/artifact/decisions-md#b-q-doc-plain records.
MD
  seed_upload notes_a "$a" notes.md "$seed_dir/notes-a.md"
  seed_request agent POST "/api/v1/issues/$a/comments" \
    "$(jq -cn --arg b "Which reader comes first is settled in dispatch://$a/artifact/decisions-md#b-q-doc-plain." \
      --argjson a "$(seed_actor)" '{body: $b, actor: $a}')"
  seed_want 201 "comment citing decisions-md#b-q-doc-plain"
  seed_field "comment citing decisions-md#b-q-doc-plain" .id
  seed_record comment_decisions_block "$seed_value"
  seed_request agent POST "/api/v1/issues/$a/comments" \
    "$(jq -cn --arg b "The format was asked in dispatch://$a/spec#b-q-left-format." \
      --argjson a "$(seed_actor)" '{body: $b, actor: $a}')"
  seed_want 201 "comment citing spec#b-q-left-format"
  seed_field "comment citing spec#b-q-left-format" .id
  seed_record comment_old_spec_address "$seed_value"
  seed_wait_settled "the decisions documents settle"

  # The sections a driver reads by their heading block, and the person's standing Keep.
  seed_decision_block "read decisions.md's sections" "$a" "Readings come from the seeded database"
  seed_record section_left_store "$seed_decision_id"
  seed_decision_block "read decisions.md's sections" "$a" \
    "How a reading is written: dispatch://$a/ask/${seed_ids[ask_left_format]}"
  seed_record section_left_format "$seed_decision_id"
  seed_decision_block "read decisions.md's sections" "$a" "Files stay out of the corpus"
  seed_record section_unsure "$seed_decision_id"
  seed_decision_block "read decisions.md's sections" "$a" "Files held the corpus once"
  seed_record section_replaced "$seed_decision_id"
  seed_decision_block "read decisions.md's sections" "$a" "The census runs before the view"
  seed_record section_kept "$seed_decision_id"
  seed_request person POST "/api/v1/artifacts/$decisions_a/blocks/$seed_decision_id/keep" \
    "$(jq -cn --arg s "$seed_decision_sum" '{text_sha256: $s}')"
  seed_want 200 "$seed_person keeps \"The census runs before the view\""
  seed_record keep_text_sha256 "$seed_decision_sum"

  # The reviews, last, each at its document's newest version.
  seed_review review_spec_a "$spec_a" approved
  seed_review review_spec_b "$spec_b" approved
  seed_review review_spec_c "${seed_ids[spec_c]}" changes_requested \
    "Say which issue the fourth one would be before asking."
  seed_review review_notes_a "${seed_ids[notes_a]}" approved
  seed_wait_settled "nothing is left to settle after the reviews"

  # Each approval still stands: no version landed after the one it names.
  local document version
  for document in "${!seed_approved_version[@]}"; do
    seed_request agent GET "/api/v1/artifacts/$document/text"
    seed_want 200 "read $document's newest version"
    seed_field "read $document's newest version" .version
    version="$seed_value"
    [[ "$version" == "${seed_approved_version[$document]}" ]] ||
      fail seed "the approval of $document stands" \
        "it names version ${seed_approved_version[$document]}, and the newest is $version"
  done

  # The corpus's own counts.
  local standing=0 index=0 collision
  for document in "${!seed_approved_version[@]}"; do
    [[ -z "${seed_ask_documents[$document]:-}" ]] || standing=$((standing + 1))
  done
  seed_record corpus_documents_holding_an_ask "${#seed_ask_documents[@]}"
  seed_record corpus_standing_approvals_over_those_documents "$standing"
  seed_record corpus_colliding_block_ids "${#seed_collisions[@]}"
  for collision in "${seed_collisions[@]}"; do
    index=$((index + 1))
    seed_record "corpus_collision_$index" "$collision"
  done
}
