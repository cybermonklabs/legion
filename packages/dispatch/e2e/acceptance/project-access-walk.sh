# shellcheck shell=bash
# shellcheck disable=SC2154 # every variable read here and assigned nowhere here is project-access.sh's or access-lib.sh's
# The scenario of project-access.sh (c-private-unseen) and its walk: what the seed writes on main
# and on the twin, the objects of PRIV and PUB a Probe's wildcards are filled with, and the walk
# that holds every row of the route index to what a missing object answers, or to what the twin
# answers. Sourced by project-access.sh alone, after scripts/e2e/lib/two-builds.sh and
# access-lib.sh, and never run: sourcing it defines functions and the tables they read, and sends
# nothing. Beside what access-lib.sh reads of its caller, it reads root, the repository's root;
# fail, note and unrun, which report; and, once project-access.sh's services section has run,
# build_url's main and twin, main_database_url and twin_database_url.

# The kinds of object a Probe fills a wildcard with (read_classes.go's probeObjects, and
# write_probes.go's for the kinds only a write's path names), each with the id that does not exist
# that fills it on the missing side. PRIV's own object of each kind is priv_object's.
declare -A missing=(
  [project]=ZZNONE
  [issue]=ZZNONE-1
  [document]=00000000-0000-4000-8000-000000000000
  [document-slug]=no-such-document
  [document-version]=1
  [block]=b-nosuchblock
  [ask]=00000000-0000-4000-8000-000000000001
  [comment]=00000000-0000-4000-8000-000000000002
  [message]=00000000-0000-4000-8000-000000000003
  [handoff]=00000000-0000-4000-8000-000000000004
  [handoff-phase]=plan
  [session]=ses-nosuchsession
  [person]=nobody@example.com
  [broadcast]=00000000-0000-4000-8000-000000000005
  [going-run]=00000000-0000-4000-8000-000000000006
  [repository-owner]=acme
  [repository-name]=no-such-repo
  [result]=00000000-0000-4000-8000-000000000007
  [thread]=00000000-0000-4000-8000-000000000008
  [attempt]=1
  [email]=nobody@example.com
  [group]=no-such-group
  [agent-token]=00000000-0000-4000-8000-000000000009
  [near-copy-check]=00000000-0000-4000-8000-00000000000a
  # A repository no issue of any project names, for a row whose body names an external reference
  # (write_probes.go's probeMappedRepository). PRIV's is acme/priv-repo, PRIV-1's own.
  [mapped-repository]=no-such-repo
  # A step record's id no run holds (write_probes.go's probeStep); the issue the path names decides
  # what is hidden, so PRIV's side names the same id.
  [step]=00000000-0000-4000-8000-00000000000b
  # A run of the deploy repository no run holds. PRIV's is priv_run, the PR-checks run the seed
  # writes of the deploy repository's pull request naming PRIV-1.
  [delivery-run]=731000009
  # A priority nobody wrote. A priority is a node of no project (no_project_kinds), filled alike on
  # both servers with the one both hold.
  [priority]=no-such-priority
)
readonly priv_run=731000001
# The kinds no project owns and no seed here writes: a Delivery session group, and the secrets
# broker's credential requests, grants, machine logins and sign-ins, which no broker of this run
# holds (a live-write pause point is a test hook's). A row that names one reads or writes the same
# object for every caller, so it has no object of PRIV to hide, and the walk names it rather than
# compares it.
readonly projectless_kinds=" live-pause session-group credential-request credential-grant machine-login signin "
# The kinds of no-project node a Probe names (spec v34, Design: "A node of no project"): both
# servers hold the same ones, and the walk fills them alike on both sides. A personal token is a
# row of its person's, which both servers hold under one label; only its list can name PRIV. A
# priority is the deployment's, access-work on both servers; only its history names an issue of
# PRIV, PRIV-2's advance of it.
readonly no_project_kinds=" person agent-token priority "

# priv_object KIND PATTERN prints PRIV's object of KIND in main's seed for a row of PATTERN, or
# nothing when the seed holds none. A document is PRIV-1's spec, whose blocks the seed knows; a
# document's slug is the slug of a document of what the path names: the project's playbook under
# /projects/, the issue's notes under /issues/, the file of the conversation under /agents/. A
# session is PRIV's, ses-priv-session, whose credential lists PRIV, and a broadcast and a file are
# of its conversation.
priv_object() {
  case "$1" in
    project) printf PRIV ;;
    issue) printf PRIV-1 ;;
    document) recalled main "PRIV spec" ;;
    document-slug)
      case "$2" in
        /api/v1/projects/*) printf playbook-md ;;
        /api/v1/issues/*) printf notes-md ;;
        /api/v1/agents/*) recalled main "PRIV session file slug" ;;
      esac
      ;;
    document-version) printf 1 ;;
    block) recalled main "PRIV block 1" ;;
    ask) recalled main "PRIV ask" ;;
    comment) recalled main "PRIV comment" ;;
    message | thread) recalled main "PRIV message" ;;
    handoff) recalled main "PRIV handoff" ;;
    result) recalled main "PRIV result" ;;
    going-run) recalled main "PRIV going run" ;;
    near-copy-check) recalled main "PRIV near-copy check" ;;
    group) printf priv-group ;;
    broadcast) recalled main "PRIV broadcast" ;;
    attempt) printf 1 ;;
    email) printf '%s' "$bob" ;;
    # The label resolve fills on each server with that server's own token.
    agent-token) printf '{carol-priv-token id}' ;;
    handoff-phase) printf plan ;;
    session) printf ses-priv-session ;;
    person) printf '%s' "$bob" ;;
    repository-owner) printf acme ;;
    repository-name) printf priv-repo ;;
    mapped-repository) printf priv-repo ;;
    step) printf 00000000-0000-4000-8000-00000000000b ;;
    delivery-run) printf '%s' "$priv_run" ;;
    priority) printf access-work ;;
  esac
}

# pub_object KIND PATTERN prints PUB's object of KIND for a row of PATTERN as a path names it on
# both servers: a literal both hold alike, or a {LABEL} resolve fills with each server's own id.
# It prints nothing for a kind PUB holds none of: a handoff, a check result, a going run, a
# near-copy check, a broadcast, a run of the deploy repository, a file of a conversation, a step.
# PUB's comment is the one that names PRIV-1 and its spec.
pub_object() {
  case "$1" in
    project) printf PUB ;;
    issue) printf PUB-1 ;;
    document) printf '{PUB spec}' ;;
    document-slug)
      case "$2" in
        /api/v1/projects/*) printf playbook-md ;;
        /api/v1/issues/*) printf notes-md ;;
      esac
      ;;
    document-version | attempt) printf 1 ;;
    block) printf '{PUB block 1}' ;;
    ask) printf '{PUB ask}' ;;
    comment) printf '{PUB reference}' ;;
    message | thread) printf '{PUB message}' ;;
    handoff-phase) printf plan ;;
    session) printf ses-pub-session ;;
    repository-owner) printf acme ;;
    repository-name | mapped-repository) printf pub-repo ;;
    group | email | agent-token | person | priority) priv_object "$1" "$2" ;;
  esac
}

# mark_restricted BUILD PERSON makes PERSON a restricted member. No route of this stack writes a
# member kind: the outside sign-in does, when it signs in an email outside the
# deployment's domain that a project names. Until it is in this stack, the people row is written
# as that sign-in writes it.
mark_restricted() {
  local url_var=${1}_database_url
  psql "${!url_var}" -X -q -v ON_ERROR_STOP=1 \
    -c "update people set member_kind = 'restricted' where email = '$(email_of "$2")'" >/dev/null ||
    fail "$1: could not make $2 a restricted member"
}

# give_repository BUILD ISSUE REPOSITORY sets ISSUE's own repository, as carol at the issue: a
# repository belongs to the projects whose issues name it, and no mapping is written.
give_repository() {
  send "$1" carol PATCH "/api/v1/issues/$2" "$(jq -cn --arg r "$3" '{repo: $r}')"
  expect_status 200 "$1: carol names $3 as the repository of $2"
}

# seed_scenario writes the seed project-access.sh's header describes, through the routes but where
# a line says why not: PUB, its people, tokens and objects alike on both servers, then PRIV and its
# objects on main alone, then the tokens that list PRIV.
seed_scenario() {
  local server document index block person uuid near_copy thread
  # PUB first, in both, so every id and event number the twin writes is the one main writes for
  # the same object.
  for server in main twin; do
    seed_people "$server"
    make_project "$server" PUB
    seed_objects "$server" PUB alice
    # The blocks of PUB's notes and playbook, which the walk of PUB's rows reads under each
    # document's slug, each remembered as seed_objects remembers the spec's.
    for document in notes playbook; do
      send "$server" carol GET "$(resolve "$server" "/api/v1/artifacts/{PUB $document}/blocks")"
      expect_status 200 "$server: carol reads the blocks of PUB's $document"
      index=0
      while IFS= read -r block; do
        index=$((index + 1))
        remember "$server" "PUB $document block $index" "$block"
      done < <(jq -r '.[].id' "$work/body")
    done
    give_repository "$server" PUB-1 acme/pub-repo
    mint_token "$server" carol carol-token
    send "$server" carol POST /api/v1/agents/ses-pub-session/messages \
      '{"body":"A direct message to the PUB session.","delivery":"btw"}'
    expect_status 201 "$server: carol messages the PUB session"
    remember "$server" "PUB direct message" "$(jq -r .id "$work/body")"
    # The reference from PUB to PRIV: carol's comment on PUB-1 names PRIV's issue and its spec. It is
    # written before main holds PRIV, so it is the same comment on both servers, with the same event
    # number; once main holds PRIV it names PRIV's objects, and on the twin keys nobody holds.
    send "$server" carol POST /api/v1/issues/PUB-1/comments \
      '{"body":"This waits on dispatch://PRIV-1 and on its spec, dispatch://PRIV-1/spec."}'
    expect_status 201 "$server: carol comments on PUB-1, naming PRIV-1 and its spec"
    remember "$server" "PUB reference" "$(jq -r .id "$work/body")"
    # An access group, the deployment's, alike on both servers, which PRIV's list names on main below.
    send "$server" carol PUT /api/v1/access-groups/priv-group "$(jq -cn --arg c "$carol" '{members: [$c]}')"
    [[ "$status" == 2?? ]] || fail "$server: carol makes the group priv-group: HTTP $status $(head -c 300 "$work/body")"
    # Delivery, configured alike on both servers, so a delivery route answers a timeline rather than
    # 404 DELIVERY_NOT_CONFIGURED; and a priority, which no project owns, alike on both.
    send "$server" carol PUT /api/v1/settings/delivery \
      '{"deploy_repo":"acme/pub-repo","deploy_workflow_path":".github/workflows/deploy.yml","production_job_names":["release"],"pr_checks_workflow_path":".github/workflows/ci.yml","population_authors":["octocat"]}'
    expect_status 200 "$server: carol configures delivery"
    send "$server" carol POST /api/v1/priorities '{"id":"access-work","title":"The access work"}'
    [[ "$status" == 2?? ]] || fail "$server: carol writes the priority access-work: HTTP $status $(head -c 300 "$work/body")"
  done
  # bob is a person, not an object PRIV wrote, so both servers hold him alike.
  mark_restricted main bob
  mark_restricted twin bob
  make_project main PRIV
  for person in "$bob" "$carol"; do
    send main carol PUT "/api/v1/projects/PRIV/access/people/$(jq -rn --arg v "$person" '$v|@uri')"
    expect_status 200 "main: carol names $person on PRIV's list"
  done
  seed_objects main PRIV bob
  give_repository main PRIV-1 acme/priv-repo
  send main carol PUT /api/v1/projects/PRIV/access/groups/priv-group
  expect_status 200 "main: carol names the group priv-group on PRIV's list"
  # ses-priv-worker's history in PRIV, which shows it to bob and carol's PRIV token (confined). Only
  # a session its own credential proves records history (events.queueSessionProjects: a
  # broker-issued token's session; the shared token only names one), and this stack has no broker.
  # So the row is a stand-in for a broker-proven session's write, as --private-by-sql's store write
  # stands in for the visibility route: it is written as that write writes it, and the stand-in goes
  # once the stack can mint a broker-issued session token. No event names the session, so it is no
  # node.
  psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 \
    -c "insert into session_projects (session_id, project_key) values ('ses-priv-worker', 'PRIV')" >/dev/null ||
    fail "main: could not record ses-priv-worker's history in PRIV"
  # The objects of PRIV the walk's Probes name beyond seed_objects': a broadcast to PRIV's session
  # and a file of its conversation, a handoff, a check result and a going run. Every uuid the
  # broadcast's answer names is remembered, in order, so the mask names each.
  send main carol POST /api/v1/broadcasts '{"body":"A broadcast to the PRIV session.","delivery":"btw","session_ids":["ses-priv-session"]}'
  expect_status 201 "main: carol broadcasts to the PRIV session"
  remember main "PRIV broadcast" "$(jq -r .id "$work/body")"
  index=0
  while IFS= read -r uuid; do
    index=$((index + 1))
    remember main "PRIV broadcast uuid $index" "$uuid"
  done < <(jq -r --arg id "$(recalled main "PRIV broadcast")" '[.. | strings | select(test("^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$") and . != $id)]
    | reduce .[] as $u ([]; if index([$u]) then . else . + [$u] end) | .[]' "$work/body")
  printf '\x89PNG\r\n\x1a\nproject-access' >"$work/session-file.png"
  status=$(curl -sS -o "$work/body" -w '%{http_code}' --max-time "$call_seconds" -H "@$work/auth/main-shared" \
    -F "name=priv-session-file.png" -F "actor=$(jq -cn --arg s "$seed_session" '{kind: "session", id: $s}')" \
    -F "file=@$work/session-file.png;type=image/png" "${build_url[main]}/api/v1/agents/ses-priv-session/artifacts" 2>"$work/curl") ||
    status="curl failed: $(<"$work/curl")"
  expect_status 201 "main: the shared token sends a file in the PRIV session's conversation"
  remember main "PRIV session file slug" "$(jq -r '.artifact.slug' "$work/body")"
  remember main "PRIV session file" "$(jq -r '.artifact.id' "$work/body")"
  send main shared POST /api/v1/issues/PRIV-1/handoffs/plan "$(with_actor shared '{"body":"{\"next\":\"seeded\"}"}')"
  expect_status 201 "main: the shared token records a handoff on PRIV-1"
  remember main "PRIV handoff" "$(jq -r '.id // .handoff.id' "$work/body")"
  send main carol POST /api/v1/issues/PRIV-1/artifacts "$(jq -cn '{name: "verification.md", content: ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe server records a result against a check.\n:::\n"}')"
  expect_status 201 "main: carol uploads PRIV-1's Verification document"
  send main shared POST /api/v1/issues/PRIV-1/results \
    "$(with_actor shared '{"check":"k1","role":"implementer","verdict":"pass","commit":"1111111111111111111111111111111111111111"}')"
  expect_status 201 "main: the shared token records a result of PRIV-1's check"
  remember main "PRIV result" "$(jq -r '.id // .result.id' "$work/body")"
  send main carol POST /api/v1/going/requests '{"words":"What does PRIV hold?","project":"PRIV"}'
  if [ "$status" = 201 ]; then
    remember main "PRIV going run" "$(jq -r .id "$work/body")"
  else
    note "no going run of PRIV: POST /api/v1/going/requests answered HTTP $status $(head -c 200 "$work/body")"
  fi
  # The objects of PRIV the query walk's fills select (walk_queries), so each answers carol
  # otherwise than a missing one does: a child of PRIV-1 that advances the priority, a pull request
  # of PRIV's repository naming PRIV-1, and a thread of carol's on PRIV-1 and one on its spec, each
  # of which bob answered.
  send main carol POST /api/v1/issues '{"project":"PRIV","title":"A child of PRIV-1","parent":"PRIV-1","advances":["access-work"]}'
  expect_status 201 "main: carol makes PRIV-2 under PRIV-1"
  # The near-copy gate answered that create and records its answer as a row of PRIV's
  # (recordNearCopyCheck, written after the create commits): the one object of PRIV that
  # GET /api/v1/near-copy-checks/{id} names. carol, who sees PRIV, reads it back from PRIV's page.
  near_copy=''
  for _ in $(seq 1 20); do
    send main carol GET '/api/v1/near-copy-checks?project=PRIV'
    near_copy=$(jq -r '[.checks[]? | select(.issue == "PRIV-2") | .id][0] // empty' "$work/body")
    [ -z "$near_copy" ] || break
    sleep 0.5
  done
  remember main "PRIV near-copy check" "$near_copy"
  psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "insert into delivery_pull_requests
      (repo, number, title, url, author, created_at, merged_at, issue_key)
    values ('acme/priv-repo', 7, 'The change of PRIV-1', 'https://github.com/acme/priv-repo/pull/7', 'octocat',
      now() - interval '3 hours', now() - interval '1 hour', 'PRIV-1')" >/dev/null ||
    fail "main: could not write the pull request of PRIV's repository"
  # The run of PRIV that GET /api/v1/delivery/runs/{id} names: a PR-checks run of the deploy
  # repository is shown where its pull request is (delivery.go's deliveryRunShown), and the pull
  # request where its issue is, so a run of a pull request naming PRIV-1 is PRIV's. The delivery loop
  # writes both rows from GitHub, which the fake GitHub does not serve, so they are written as it
  # writes them, the run's jobs complete.
  psql "$main_database_url" -X -q -v ON_ERROR_STOP=1 -c "insert into delivery_pull_requests
      (repo, number, title, url, author, created_at, merged_at, issue_key)
    values ('acme/pub-repo', 8, 'A change for PRIV-1', 'https://github.com/acme/pub-repo/pull/8', 'octocat',
      now() - interval '3 hours', now() - interval '2 hours', 'PRIV-1')" \
    -c "insert into delivery_runs
      (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, completed_at, conclusion, url, jobs_complete)
    values ('acme/pub-repo', $priv_run, 'pr_checks', 8, '2222222222222222222222222222222222222222',
      now() - interval '3 hours', now() - interval '3 hours', now() - interval '170 minutes', 'success',
      'https://github.com/acme/pub-repo/actions/runs/$priv_run', true)" >/dev/null ||
    fail "main: could not write the deploy repository's pull request naming PRIV-1 and its PR-checks run"
  # A thread on PRIV-1, and one on PRIV's playbook, a document of the project, which takes its
  # comments on its own route (a document of an issue takes them through the issue's).
  for thread in /api/v1/issues/PRIV-1/comments "/api/v1/artifacts/$(recalled main "PRIV playbook")/comments"; do
    send main carol POST "$thread" '{"body":"carol asks something here."}'
    expect_status 201 "main: carol writes a thread at $thread"
    send main bob POST "$thread" "$(jq -cn --arg r "$(jq -r .id "$work/body")" '{body: "bob answers carol.", reply_to: $r}')"
    expect_status 201 "main: bob answers carol's thread at $thread"
  done
  # carol's token that lists PRIV is minted while PRIV is public. No personal token lists a private
  # project whose key DISPATCH_PRIVATE_PROJECTS names (R18), so once PRIV is private this token is
  # revoked (project-access.sh's private section) and reads only while PRIV is public: under
  # --all-public, in the listed and confined sections. No token of alice's lists PRIV: under R18 no
  # credential of a full member who cannot see a private project lists it, so that caller does not
  # exist once PRIV is private, where the walk reads.
  mint_token main carol carol-priv-token '["PRIV"]'
  # carol's token that lists PRIV is a row of carol's, a no-project object; only its list names
  # PRIV, and that list is what a reader who does not see PRIV may not read. So the twin holds the
  # same token, listing nothing, minted at the same step and used once as main's is used, and
  # revoked when main's is (project-access.sh's private section), so carol's list of her tokens
  # compares alike, its order (newest first) and its last use included.
  mint_token twin carol carol-priv-token
  for server in main twin; do
    send "$server" carol-priv-token GET /api/v1/whoami
    expect_status 200 "$server: carol's second token reads whoami"
  done
  # carol's token that lists PUB is a credential that lists projects, which the walk's rows whose
  # object no project owns refuse (it is not open). It is minted on both servers, after the token
  # above, for the same reason.
  for server in main twin; do
    mint_token "$server" carol carol-pub-token '["PUB"]'
  done
}

# The callers PRIV's list lets in, which project-access.sh's listed section reads as, and those it
# does not, whom the walk reads as. carol's PRIV token is a listed caller only while PRIV is public:
# the private section revokes it (listed_now).
# shellcheck disable=SC2034 # read by project-access.sh's listed section
readonly listed_callers=(bob carol carol-priv-token)
readonly unseeing_callers=(alice carol-token shared)
# The object routes the listed callers read, each naming one object of {P}'s seed.
# shellcheck disable=SC2034 # read by project-access.sh's listed and confined sections
readonly object_reads=(
  "/api/v1/projects/{P}/artifacts"
  "/api/v1/issues/{P}-1"
  "/api/v1/comments/{{P} comment}"
  "/api/v1/asks/{{P} ask}"
  "/api/v1/messages/{{P} message}"
  "/api/v1/artifacts/{{P} spec}"
  "/api/v1/artifacts/{{P} notes}"
  "/api/v1/artifacts/{{P} playbook}"
)

# compare_missing CALLER METHOD TEMPLATE PRIV_PATH MISSING_PATH PRIV_MASK MISSING_MASK [BODY]: as
# CALLER on main, the request to PRIV_PATH and the one to MISSING_PATH answer the same, each
# masked with its own fill (the files PRIV_MASK and MISSING_MASK).
compare_missing() {
  local caller=$1 method=$2 template=$3
  send main "$caller" "$method" "$4" "${8:-}"
  keep_answer hidden main "$6"
  send main "$caller" "$method" "$5" "${8:-}"
  keep_answer missing main "$7"
  same_answers "$caller $method $template" hidden missing
}

# Every answer says whether the deployment holds a private project (KF7): main, which provisions
# PRIV, says 1, and the twin, which provisions none, 0. Beside the twin, each server's answer must
# say its own, and the line is dropped from both before the headers are compared.
readonly main_header='dispatch-private-projects: 1' twin_header='dispatch-private-projects: 0'

# compare_twin CALLER PATH [METHOD BODY]: as CALLER, main and the twin answer PATH the same, each
# with its own ids filled in for the {LABEL}s PATH names. A write (METHOD other than GET) writes a
# row of its own on each server, whose new id and time no seed names, so of a write the two
# statuses alone are held alike.
compare_twin() {
  local caller=$1 path=$2 method=${3:-GET} body=${4:-} server
  for server in main twin; do
    send "$server" "$caller" "$method" "$(resolve "$server" "$path")" "$body"
    keep_answer "$server" "$server"
  done
  if [ "$method" != GET ]; then
    for server in main twin; do
      : >"$work/answers/$server.body"
      : >"$work/answers/$server.headers"
    done
    same_answers "$caller $method $path (beside the twin)" main twin
  else
    same_answers "$caller $method $path (beside the twin)" main twin "$main_header" "$twin_header"
  fi
}

# Each server's delivery loop reconciles on its own timer once delivery is configured (every second
# here, DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL, rather than five minutes), and a reconcile
# writes what it found into the settings and the pipeline's freshness. So both servers are read
# only once each has reconciled at least once, and their answers hold the same outcome.
reconciled() {
  local server
  for server in main twin; do
    send "$server" carol GET /api/v1/settings/delivery
    [ "$status" = 200 ] && [ "$(jq '.last_error != null or .last_reconcile_at != null' "$work/body")" = true ] || return 1
  done
}

# Each server pages its event log only up to its safe horizon (listEventLog, sinceHorizon), and a
# server can answer a read before that horizon has passed what the seed wrote, so one server's log
# would read short beside the other's. The walk compares the log as each unseeing caller, so each
# server is read only once its log, as each caller that wrote in the seed as itself reads it, holds
# that caller's own newest event, which a caller always reads: carol, who wrote last, and alice. A
# token wrote nothing, and the shared token's writes name a session it does not prove and are in
# PRIV, which it does not see. After carol's newest event the seed writes only bob's answers in
# PRIV, which no unseeing caller reads.
#
# own_newest_event BUILD PERSON prints the id of the newest event of BUILD's database that PERSON
# wrote as themself, or 0 when they wrote none.
own_newest_event() {
  local url_var=${1}_database_url
  psql "${!url_var}" -X -tAc "select coalesce(max(id), 0) from events
    where actor->>'kind' = 'user' and actor->>'id' = '$(email_of "$2")'"
}

# logs_hold_own_writes waits, bounded, on each server until the log as carol and as alice holds
# that caller's own newest event.
logs_hold_own_writes() {
  local server caller newest
  for server in main twin; do
    for caller in carol alice; do
      newest=$(own_newest_event "$server" "$caller") || fail "$server: could not read $caller's newest event"
      [ "$newest" -gt 0 ] || continue
      until_true 120 "$server's event log, as $caller reads it, to hold $caller's newest event, $newest" \
        event_log_reaches "$server" "$caller" "$newest"
    done
  done
}

# route_rows SELECT prints, in their order, the rows of the route index that the jq filter SELECT
# keeps, each with what its Probe (RouteProbe, read once from probes.json) says of it, one line a
# row and its fields joined by the unit separator (\x1f), a byte no compact JSON holds: the method;
# the path; 1 when a Probe names the row; its test_hook; its no_project; its body as compact JSON;
# 1 when its Probe map is not null; that map, each wildcard and its kind joined by \x1d and the
# pairs by \x1e (read_probe); and its read classes, joined by commas. A field the Probe does not
# hold is empty.
route_rows() {
  jq -r --slurpfile probes "$work/probes.json" "$1"' | . as $row
    | (first($probes[0][] | select(.method == $row.method and .pattern == $row.path)) // null) as $entry
    | [$row.method, $row.path,
       (if $entry == null then "" else "1" end),
       ($entry.test_hook // false | tostring),
       ($entry.no_project // ""),
       ((($entry.body // empty) | tojson) // ""),
       (if $entry.probe == null then "" else "1" end),
       ([$entry.probe // {} | to_entries[] | "\(.key)\u001d\(.value // "")"] | join("\u001e")),
       ($entry.classes // [] | join(","))]
    | join("\u001f")' "$work/routes"
}

# read_probe PAIRS sets probe_kind to the kind of each wildcard PAIRS names (route_rows' eighth
# field).
declare -A probe_kind=()
read_probe() {
  local pair
  probe_kind=()
  while IFS= read -r -d $'\x1e' pair; do
    probe_kind[${pair%%$'\x1d'*}]=${pair#*$'\x1d'}
  done <<<"${1:+$1$'\x1e'}"
}

# walk_rows SELECT walks the rows of the route index that the jq filter SELECT keeps: a row whose
# path names an object is filled from its Probe with PRIV's objects and with missing ones, and
# each unseeing caller is answered the same for both; a GET row that names no object is a list,
# answered as the twin answers it. A row that names an object with no Probe, or one whose object
# the seed does not hold, is written to what did not run. A test hook (RouteProbe's test_hook) is
# left out, named: the server mounts it only under DISPATCH_TEST_HOOKS, which no deployment sets.
# A person, a personal token and a priority are no-project nodes, which spec v34 (Design, "A node
# of no project") shows to an open viewer as it does today, and both servers hold the same ones: a
# row whose every wildcard is one of them is filled alike on both servers and answered as the twin
# answers it. A GET row whose only read class is none (RouteProbe's classes) reads no row a
# caller's access decides, so it answers every caller alike, and its answer for PRIV's object can
# rightly differ from its answer for a missing one, as GET
# /api/v1/github/app-installation/{owner}/{repo} answers installed:true for PRIV's repository, the
# App's, whoever asks: it is filled with PRIV's objects on both servers and answered as the twin,
# which never held PRIV, answers it. The rows whose object no project owns (RouteProbe's
# no_project) are walked first: each must be refused, which changes nothing, and a write walked
# before them would change the callers they are refused to, as DELETE /api/v1/me/agent-tokens/{id}
# does, which carol's token walks on carol's PRIV token.
walk_rows() {
  local method pattern has_entry test_hook no_project body has_probe pairs classes wildcards hidden none skip wildcard kind object caller target
  walked=0 lists=0 refused=0
  while IFS=$'\x1f' read -r method pattern has_entry test_hook no_project body has_probe pairs classes; do
    if [ "$test_hook" = true ]; then
      note "left out: $method $pattern is a test hook, which only DISPATCH_TEST_HOOKS mounts"
      continue
    fi
    if [ -n "$no_project" ]; then
      # The object belongs to no project, so there is no object of PRIV to hide: the row is held
      # to its class's refusal (scopeDeployment) of every caller that is not open.
      target=$(sed -E 's/\{[^}]+\}/project-access-walk/g' <<<"$pattern")
      for caller in bob carol-pub-token; do
        send main "$caller" "$method" "$target" "$body"
        refused=$((refused + 1))
        problem=''
        [ "$status" = 403 ] ||
          problem="$caller $method $pattern ($no_project): HTTP $status, want 403 for a caller that is not open: $(head -c 300 "$work/body")"
        judge "$status" "$problem"
      done
      continue
    fi
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    if [ -z "$wildcards" ]; then
      if [ "$method" = GET ]; then
        for caller in "${unseeing_callers[@]}"; do compare_twin "$caller" "$pattern"; done
        lists=$((lists + 1))
      fi
      continue
    fi
    if [ -z "$has_entry" ] || [ -z "$has_probe" ]; then
      unrun "$method $pattern names an object and has no Probe"
      continue
    fi
    read_probe "$pairs"
    hidden=$pattern
    none=$pattern
    : >"$work/fill-hidden"
    : >"$work/fill-missing"
    skip=''
    for wildcard in $wildcards; do
      kind=${probe_kind[$wildcard]:-}
      if [[ "$projectless_kinds" == *" $kind "* ]]; then
        skip=projectless
        note "not walked: $method $pattern: {$wildcard} is a $kind, which no project owns"
        break
      fi
      object=$(priv_object "$kind" "$pattern")
      if [ -z "$object" ] || [ -z "${missing[$kind]:-}" ]; then
        skip=unseeded
        unrun "$method $pattern: the seed holds no ${kind:-object} of PRIV for {$wildcard}"
        break
      fi
      # A no-project node fills both sides alike: it is no object PRIV wrote. It goes into the
      # path as it is, so a {LABEL} it names is filled with each server's own id (compare_twin).
      if [[ "$no_project_kinds" == *" $kind "* ]]; then
        hidden=${hidden//"{$wildcard}"/$object}
        none=${none//"{$wildcard}"/$object}
        continue
      fi
      hidden=${hidden//"{$wildcard}"/$(jq -rn --arg v "$object" '$v|@uri')}
      none=${none//"{$wildcard}"/$(jq -rn --arg v "${missing[$kind]}" '$v|@uri')}
      # A fill that is the same on both sides is no difference to mask. An issue's key names its
      # project up to its first -, which a refusal built from the key the caller sent names
      # (ROLE_HOLDER_FORBIDDEN): it is masked too, after the key, so a hidden answer naming the
      # project and a missing one naming ZZNONE read alike, and an answer naming another object
      # of PRIV still differs.
      if [ "$object" != "${missing[$kind]}" ]; then
        printf '%s\t%s\n' "$wildcard" "$object" >>"$work/fill-hidden"
        printf '%s\t%s\n' "$wildcard" "${missing[$kind]}" >>"$work/fill-missing"
        if [ "$kind" = issue ]; then
          printf '%s project\t%s\n' "$wildcard" "${object%%-*}" >>"$work/fill-hidden"
          printf '%s project\t%s\n' "$wildcard" "${missing[$kind]%%-*}" >>"$work/fill-missing"
        fi
      fi
    done
    [ -z "$skip" ] || continue
    if [ "$hidden" = "$none" ] || [ "$classes" = none ]; then
      # Every wildcard is a no-project node, so the path is the same on both servers; or the row
      # reads no row a caller's access decides, and both servers are read with PRIV's objects.
      for caller in "${unseeing_callers[@]}"; do
        compare_twin "$caller" "$hidden" "$method" "$body"
      done
      lists=$((lists + 1))
      continue
    fi
    for caller in "${unseeing_callers[@]}"; do
      compare_missing "$caller" "$method" "$pattern" "$hidden" "$none" "$work/fill-hidden" "$work/fill-missing" "$body"
    done
    walked=$((walked + 1))
  done < <(route_rows "$1" | LC_ALL=C sort -s -r -t $'\x1f' -k5,5)
}

# walk_pub_rows walks every GET row whose path names an object of a project, filled with PUB's
# objects (pub_object) alike on both servers, and holds each unseeing caller's answer to the
# twin's. PUB is the same on both, but for the reference from PUB to PRIV, which on main names
# PRIV's objects and on the twin keys nobody holds: a read of PUB that names an object of PRIV
# through it, or answers otherwise for it, differs. A row whose object PUB holds none of is not
# walked here, and one whose every wildcard is a no-project node is walked by walk_rows.
walk_pub_rows() {
  local pattern test_hook pairs wildcards wildcard kind object path of_project caller
  pub_walked=0
  while IFS=$'\x1f' read -r _ pattern _ test_hook _ _ _ pairs _; do
    [ "$test_hook" != true ] || continue
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    [ -n "$wildcards" ] || continue
    read_probe "$pairs"
    path=$pattern of_project=''
    for wildcard in $wildcards; do
      kind=${probe_kind[$wildcard]:-}
      object=$(pub_object "$kind" "$pattern")
      if [ -z "$object" ]; then
        path=''
        break
      fi
      [[ "$no_project_kinds" == *" $kind "* ]] || of_project=1
      path=${path//"{$wildcard}"/$object}
    done
    [ -n "$path" ] && [ -n "$of_project" ] || continue
    for caller in "${unseeing_callers[@]}"; do
      compare_twin "$caller" "$path"
    done
    pub_walked=$((pub_walked + 1))
  done < <(route_rows 'select(.method == "GET")')
}

# The query parameters of the GET rows, read from the OpenAPI this checkout's server serves (`go run
# ./cmd/dispatch openapi --check` holds the checked-in document equal to the route table), each
# classified once. A parameter that names an object of a project is filled with PRIV's and with a
# missing one, as a path's wildcard is, and each unseeing caller is answered the same for both; one
# that names a person, a node of no project both servers hold, or a session, filled with PRIV's,
# which the twin does not hold, is answered as the twin answers it; one that names no object is
# not filled. A parameter these tables do not classify did not run, so a new one cannot pass
# unread. A key "ROUTE NAME" classifies NAME on ROUTE alone, ahead of NAME.
declare -A query_kind=(
  [project]=project
  [number_project]=project
  [issue]=issue
  [parent]=issue
  [ref]=issue
  [artifact]=document
  [repo]=repository
  # Of these routes, from and to name nodes, written as dispatch:// references; of every other
  # route that takes them, times.
  ["/api/v1/block-links from"]=spec-ref
  ["/api/v1/block-links to"]=spec-ref
  ["/api/v1/references from"]=issue-ref
  ["/api/v1/references to"]=issue-ref
  # What many references point at: each ref is a dispatch:// reference, as the two above.
  ["/api/v1/reference-targets ref"]=issue-ref
  # The question map's id names a comment once its kind says so (query_fixed).
  ["/api/v1/references/questions id"]=comment
  # An issue's comments are those of the path's issue, so ?artifact= names a document of that
  # issue, and a document of PRIV goes under a path of PRIV, which the path walk answers as missing.
  ["/api/v1/issues/{key}/comments artifact"]=none
)
# What a route, or a "ROUTE NAME", needs beside the parameter filled: a document at the other end
# of a block link, a kind for the question map, the text of a search.
declare -A query_fixed=(
  ["/api/v1/references/questions"]='kind=comment&'
  ["/api/v1/block-links from"]='to=dispatch%3A%2F%2FPUB-1%2Fspec&'
  ["/api/v1/block-links to"]='from=dispatch%3A%2F%2FPUB-1%2Fspec&'
  ["/api/v1/search"]='q=paragraph&'
  ["/api/v1/search/results"]='q=paragraph&'
)
readonly query_no_project=" author author_session parent_agent assignee approver person "
# exclude_issue and exclude_issues name an issue of the path's own project, so an issue of PRIV
# goes under a path of PRIV, which the path walk answers as missing; thread is a flag (1 or none).
readonly query_no_object=" advances after at before blocks closed_before closed_since component cursor deployed download exclude_issue exclude_issues from ids keywords_only kind label limit max_chars moved offset open order pinned priority q rework route_status session since source_kind state status summary text thread title to unlinked until updated_since versions wait "

# query_fill KIND prints PRIV's object of KIND and the missing one, a tab between them.
query_fill() {
  case "$1" in
    project) printf 'PRIV\t%s' "${missing[project]}" ;;
    issue) printf 'PRIV-1\t%s' "${missing[issue]}" ;;
    issue-ref) printf 'dispatch://PRIV-1\tdispatch://%s' "${missing[issue]}" ;;
    spec-ref) printf 'dispatch://PRIV-1/spec\tdispatch://%s/spec' "${missing[issue]}" ;;
    # A thread is on a document of its own route only for a project's document (the seed's
    # thread on PRIV's playbook); a document of an issue takes its comments through the issue.
    document) printf '%s\t%s' "$(recalled main "PRIV playbook")" "${missing[document]}" ;;
    comment) printf '%s\t%s' "$(recalled main "PRIV comment")" "${missing[comment]}" ;;
    repository) printf 'acme/priv-repo\tacme/%s' "${missing[repository-name]}" ;;
  esac
}

# walk_queries walks every query parameter of every GET row of the route index (query_kind). Of a
# row whose path names an object, the path is filled with PUB's, which every unseeing caller sees.
# carol, who sees PRIV, must be answered 2xx for PRIV's fill and otherwise than for the missing one,
# each masked with its own fill: a refusal says the fill is no value the parameter takes, and an
# answer alike says the fill selects nothing of PRIV, and either way a hidden and a missing answer
# would be alike whatever the server hides, so the parameter did not run.
walk_queries() {
  local pattern test_hook pairs wildcards wildcard kind path param fixed values hidden none caller
  local -A query_params=()
  queried=0
  # Each GET path's query parameters, in the OpenAPI's order, one per line, read in one pass.
  while IFS=$'\x1f' read -r pattern param; do
    query_params[$pattern]+=$param$'\n'
  done < <(jq -r '.paths | to_entries[] | .key as $p | .value.get.parameters[]? | select(.in == "query") | [$p, .name] | join("\u001f")' \
    "$root/packages/contracts/schemas/dispatch-openapi.json")
  while IFS=$'\x1f' read -r _ pattern _ test_hook _ _ _ pairs _; do
    [ "$test_hook" != true ] || continue
    read_probe "$pairs"
    path=$pattern
    wildcards=$(grep -oE '\{[^}]+\}' <<<"$pattern" | tr -d '{}' || true)
    for wildcard in $wildcards; do
      case "${probe_kind[$wildcard]:-}" in
        project) path=${path//"{$wildcard}"/PUB} ;;
        issue) path=${path//"{$wildcard}"/PUB-1} ;;
        *)
          path=''
          break
          ;;
      esac
    done
    while IFS= read -r param; do
      [ -n "$param" ] || continue
      kind=${query_kind["$pattern $param"]:-${query_kind[$param]:-}}
      [ "$kind" != none ] || continue
      if [ -z "$kind" ] && [[ "$query_no_project" != *" $param "* ]]; then
        [[ "$query_no_object" == *" $param "* ]] || unrun "GET $pattern takes ?$param=, which the query walk does not classify"
        continue
      fi
      if [ -z "$path" ]; then
        unrun "GET $pattern?$param=: the query walk cannot fill this path with PUB's objects"
        continue
      fi
      fixed=${query_fixed[$pattern]:-}
      if [ -z "$kind" ]; then
        # A person, whom both servers hold alike, or PRIV's session, which the twin does not hold:
        # either is answered as the twin answers it.
        case "$param" in
          author_session | parent_agent) kind=session ;;
          *) kind=person ;;
        esac
        for caller in "${unseeing_callers[@]}"; do
          compare_twin "$caller" "$path?$fixed$param=$(jq -rn --arg v "$(priv_object "$kind" "$pattern")" '$v|@uri')"
        done
        queried=$((queried + 1))
        continue
      fi
      values=$(query_fill "$kind")
      fixed=${query_fixed["$pattern $param"]:-${query_fixed[$pattern]:-}}
      hidden="$path?$fixed$param=$(jq -rn --arg v "${values%%$'\t'*}" '$v|@uri')"
      none="$path?$fixed$param=$(jq -rn --arg v "${values#*$'\t'}" '$v|@uri')"
      printf '%s\t%s\n' "$param" "${values%%$'\t'*}" >"$work/fill-hidden"
      printf '%s\t%s\n' "$param" "${values#*$'\t'}" >"$work/fill-missing"
      # A reference's refusal names the issue by its bare key, which the masks name too.
      if [[ "$kind" == *-ref ]]; then
        printf '%s key\tPRIV-1\n' "$param" >>"$work/fill-hidden"
        printf '%s key\t%s\n' "$param" "${missing[issue]}" >>"$work/fill-missing"
      fi
      send main carol GET "$hidden"
      keep_answer seen-hidden main "$work/fill-hidden"
      send main carol GET "$none"
      keep_answer seen-missing main "$work/fill-missing"
      if [[ "$(<"$work/answers/seen-hidden.status")" != 2* ]]; then
        unrun "GET $pattern?$param=<$kind>: carol, who sees PRIV, is answered HTTP $(<"$work/answers/seen-hidden.status") for PRIV's, so the fill is no value the parameter takes: $(head -c 200 "$work/answers/seen-hidden.body")"
        continue
      fi
      if [ "$(<"$work/answers/seen-hidden.status")" = "$(<"$work/answers/seen-missing.status")" ] &&
        cmp -s "$work/answers/seen-hidden.body" "$work/answers/seen-missing.body"; then
        unrun "GET $pattern?$param=<$kind>: carol, who sees PRIV, is answered alike for PRIV's and a missing one, so the fill selects nothing of PRIV and a leak could not show"
        continue
      fi
      for caller in "${unseeing_callers[@]}"; do
        compare_missing "$caller" GET "$pattern?$param=<$kind>" "$hidden" "$none" "$work/fill-hidden" "$work/fill-missing"
      done
      queried=$((queried + 1))
    done <<<"${query_params[$pattern]:-}"
  done < <(route_rows 'select(.method == "GET")')
}
