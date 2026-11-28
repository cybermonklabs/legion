#!/usr/bin/env bash
# Drives check c-conversation-holds-the-discussion of the issue tree's Verification document: the
# conversation graph, GET /api/v1/issues/{key}/tree, answers the discussion and nothing else. It
# serves the spec's requirements r-queries and r-conversation-parent, and the root's
# r-document-tree. Every read of the tree goes through that route.
#
# It runs against the `bun run dispatch:local` server of the Verification preamble, over
# infra_1253, as the agent bearer and as the person DEV_SIGNIN_LOGIN, whom the dev sign-in signs
# in. Sections:
#   seed        one issue, born converted, holding every node kind this model holds: a thread of
#               comments with replies the person wrote, an agent's reply inside that thread and an
#               agent's status comment at the top of the issue; an ask the agent opened and the
#               person answered; a remark of the person's that nobody answered; a criterion the
#               person wrote into the issue's tree under the thread's first block; a requirement
#               under that criterion; a decision an agent recorded with its shaped tuple to the
#               requirement, holding the agent's reading of it; a check and a plan task that serve
#               it, and a result recorded on the check; an agent's comment on the requirement's text
#               that no person answered; the spec's memo callout as its first child, its Summary
#               paragraph, and a second callout; two documents of several versions each
#               (verification.md and notes.md); a message the Legion daemon's session
#               legion-daemon:<PROJECT> posts; a message thread that session began, with a reply the
#               person wrote and one an agent wrote; the person's own message with an agent's reply
#               in its thread; the person's approval of the spec; and a child issue. It reads back
#               from the store that the issue carries a node of each of those kinds.
#   kinds       the read with no view answers, as nodes, the comments, the replies, the agent's reply
#               in the person's thread, the ask with its answer held on its node, the result and the
#               reading, the remark, the criterion, the requirement, the decision, the check and the
#               plan task; it answers no artifact, no document version and no prose block of a role
#               document, the Summary among them, which the spec's version still renders; no message
#               of the daemon's own session and no message of its thread, with no view and with
#               kind=message, while both reads answer the person's own message and its reply and the
#               issue's messages hold all four of the daemon's; not the agent's status comment, while
#               the issue's comments hold it and the reply; the agent's comment on the requirement's
#               text under the requirement as context, with no node under it; and the memo callout
#               first.
#   memo        ?view=spec answers the memo first as a node of the query and not the second callout;
#               a change to one of the memo's three sentences moves the digest the spec approval
#               recorded, and an edit of the second callout moves no digest; a third issue whose
#               first child is a paragraph and which holds a callout after it answers no callout on
#               ?view=spec.
#   large       a second issue of at least the size of the largest of the ten issues R8 measures,
#               which seed-corpus.sh's corpus_large_issue writes with its corpus_measured_*
#               constants, the counts the pull request body records, its comments raised past two
#               comment pages: its read with no view is followed by its node cursor to the end, each
#               page at most 2,000,000 uncompressed bytes, answering the skeleton - each typed block
#               of its role documents, no block of another document - with its conversation's
#               messages and its newest 200 comments; its older comments are read through
#               comments_before until null, each page the next older 200, and the pages hold each
#               comment the walk takes once; and each document document_nodes lists, each other
#               document of the issue, is read through GET /api/v1/artifacts/{id}/blocks, which
#               lists as many blocks as its block_count. It prints the page size, the node count,
#               the number of comment pages and the number of blocks it read.
#   child       the child issue's ask is a conversation node of the child's tree.
#
# A node the tree must leave out, if it is in the answer, is printed with its kind and its id, and
# the run fails; a message of the daemon's thread with its root too; a missing node by its id.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (the agent bearer, read
# here and never printed), PROJECT (CORE, which the server seeds), DEV_SIGNIN_LOGIN (alice) and
# DATABASE_URL (the database that server serves, infra_1253, which the settlement waits of
# seed-corpus.sh read). Each unmet input is exit 2.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: conversation-view.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/infra_1253?sslmode=disable \
         DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice \
         bash e2e/acceptance/conversation-view.sh
       (run from packages/dispatch against `bun run dispatch:local`; DATABASE_URL names the
       database that server serves)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

readonly driver=conversation-view.sh
require_env DATABASE_URL DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands cmp comm curl cut grep jq psql sed sort tr uniq wc
read_bearer
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
base="${DISPATCH_URL%/}"
readonly base
# shellcheck disable=SC2031 # the run's own DATABASE_URL
database_url="$DATABASE_URL"
readonly database_url
loopback_database_url "$database_url" || {
  printf '%s: DATABASE_URL names no single loopback database\n' "$driver" >&2
  exit 2
}
readonly person_login="$DEV_SIGNIN_LOGIN"
session="conversation-view-$(date +%s)-$$"
readonly session
# The Legion daemon's own session of the project, which R2 keeps out of the conversation with every
# message of a thread it began.
readonly daemon_session="legion-daemon:$project"
# The most uncompressed bytes one page of the tree read may answer (R8).
readonly page_bytes_bound=2000000
# The typed blocks of a role document; any other block of one is prose.
readonly typed_blocks='["callout","ask","requirement","plan_task","check","decision","reading","criterion"]'

readonly memo_sentence="The conversation holds the discussion." memo_changed="The conversation holds the whole discussion."
readonly second_words="A second callout, which is not the memo." second_changed="A second callout, which is never the memo."
readonly summary_words="The summary paragraph of the issue whose conversation this check reads."
readonly requirement_words="The conversation holds every message a person exchanged."
readonly remark_words="A remark nobody answered, which produced nothing."

work_dir
sections=0
keys=()
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  local closing
  for closing in "${keys[@]}"; do
    tree_send PATCH "/api/v1/issues/$closing" "$(jq -cn --arg s "$session" '{status: "done", actor: {kind: "session", id: $s}}')"
    [[ "$status" == 200 ]] || printf '%s: could not close %s: HTTP %s\n' "$driver" "$closing" "$status" >&2
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# conversation_view_create SECTION TITLE FILE [PARENT]: the agent creates an issue of `project`
# titled TITLE, FILE's markdown its spec, under PARENT where given, through seed-corpus.sh's
# corpus_issue; it is closed when the run ends.
conversation_view_create() {
  corpus_issue "$1" "$2" "$3" "${4:-}"
  tree_key "$1" "create the issue \"$2\"" "$corpus_key"
  keys+=("$corpus_key")
}

# conversation_view_message SESSION SECTION ISSUE BODY [IN_REPLY_TO]: the agent bearer posts a
# message on ISSUE naming SESSION as its actor, a reply to IN_REPLY_TO where given; sets corpus_item.
conversation_view_message() {
  tree_send POST "/api/v1/issues/$3/messages" "$(jq -cn --arg b "$4" --arg r "${5:-}" --arg s "$1" \
    '{body: $b, actor: {kind: "session", id: $s}} + (if $r == "" then {} else {in_reply_to: $r} end)')"
  tree_want 201 "$2" "$1 posts a message on $3"
  tree_field "$2" "$1 posts a message on $3" .id
  corpus_item="$tree_value"
}

# conversation_view_absent SECTION FILE WHAT KIND ID [MORE]: the read in FILE holds no node KIND:ID;
# a node it holds is printed with its kind, its id and MORE, and the run fails.
conversation_view_absent() {
  local held
  held="$(jq -c --arg k "$4" --arg i "$5" '[.nodes[] | select(.kind == $k and .id == $i) | {kind, id, role, parent}]' "$2")"
  [[ "$held" == "[]" ]] || fail "$1" "$3" "the answer holds $4 $5${6:+ ($6)}, a node the graph must not hold: $held"
  pass "$1" "$3"
}

# conversation_view_present SECTION FILE WHAT KIND ID ROLE: the read in FILE holds the node KIND:ID
# once, in the role ROLE; a missing node is printed by its id.
conversation_view_present() {
  local held
  held="$(jq -c --arg k "$4" --arg i "$5" '[.nodes[] | select(.kind == $k and .id == $i) | .role]' "$2")"
  [[ "$held" == "[\"$6\"]" ]] || fail "$1" "$3" "the answer holds the $4 node $5 as $held, want it once as $6"
  pass "$1" "$3"
}

# conversation_view_digest SECTION DOCUMENT NUMBER: the digest version NUMBER of DOCUMENT carries in
# GET /api/v1/artifacts/{id}, what an approval of that version covers, in tree_value.
conversation_view_digest() {
  tree_send GET "/api/v1/artifacts/$2"
  tree_want 200 "$1" "read the document $2"
  tree_field "$1" "the digest of version $3 of $2" "[.versions[] | select(.number == $3) | .digest][0] // empty"
}

# conversation_view_newest SECTION DOCUMENT: the number of DOCUMENT's newest version, in tree_value.
conversation_view_newest() {
  tree_send GET "/api/v1/artifacts/$2"
  tree_want 200 "$1" "read the document $2"
  tree_field "$1" "the newest version of $2" '[.versions[].number] | max'
}

# conversation_view_comment_page SECTION PAGE FILE: the comments FILE lists, which comment page PAGE
# of the large issue answered, are its comments the walk takes from (PAGE - 1) * comment_page_size
# + 1 to PAGE * comment_page_size in $work/large-ordered.txt, the order the pages read them in,
# newest first: the newest 200 for the first page, the next older 200 for each page after it.
conversation_view_comment_page() {
  local section="$1" page="$2" file="$3" from to
  from=$(((page - 1) * comment_page_size + 1))
  to=$((page * comment_page_size))
  sed -n "${from},${to}p" "$work/large-ordered.txt" | LC_ALL=C sort >"$work/large-want-$page.txt"
  LC_ALL=C sort "$file" >"$work/large-got-$page.txt"
  cmp -s "$work/large-want-$page.txt" "$work/large-got-$page.txt" ||
    fail "$section" "comment page $page answers the comments the walk takes from the ${from}th to the ${to}th newest" \
      "only among those: $(LC_ALL=C comm -23 "$work/large-want-$page.txt" "$work/large-got-$page.txt" | tr '\n' ' '); only on the page: $(LC_ALL=C comm -13 "$work/large-want-$page.txt" "$work/large-got-$page.txt" | tr '\n' ' ')"
  pass "$section" "comment page $page answers the $(wc -l <"$work/large-got-$page.txt" | tr -d ' ') comments the walk takes from the ${from}th newest on"
}

# --- setup -------------------------------------------------------------------------------------

tree_section setup
tree_init
tree_sign_in setup "$person_login"

# --- the issue that holds every node kind -----------------------------------------------------------

tree_section seed
cat >"$work/spec.md" <<SPEC
:::callout{#memo kind="note" title=""}
$memo_sentence A remark nobody answered stays a node of it. The daemon's own thread is none of it.
:::

$summary_words

:::callout{#second kind="note" title="A second callout"}
$second_words
:::
SPEC
conversation_view_create seed "The conversation holds the discussion" "$work/spec.md"
# Not readonly: seed-corpus.sh's corpus_large_issue, which the large section runs, takes a local of
# that name.
key="$corpus_key"
readonly spec="$corpus_spec"
tree_keep seed "/api/v1/issues/$key/tree" "$work/tree-first.json"
tree_kept_expect "$work/tree-first.json" seed "$key is born converted and holds its tree" '[true,true]' \
  '[.converted, (.tree | type == "string")]'
tree="$(jq -r '.tree' "$work/tree-first.json")"
readonly tree
tree_keep seed "/api/v1/artifacts/$tree" "$work/tree-artifact.json"
tree_slug="$(jq -r '.slug' "$work/tree-artifact.json")"
readonly tree_slug

# The thread a person wrote: its head, the person's reply, and an agent's reply inside it.
corpus_comment person seed "$key" "We want the conversation to hold the discussion."$'\n\n'"And to leave the workflow out of it."
readonly head="$corpus_item"
tree_keep seed "/api/v1/comments/$head/blocks" "$work/head-blocks.json"
head_block="$(jq -r '.blocks[0].ref // empty' "$work/head-blocks.json")"
[[ -n "$head_block" ]] || fail seed "read the first block of the comment $head" "no ref: $(head -c 600 "$work/head-blocks.json")"
readonly head_block
corpus_comment person seed "$key" "And the replies a person wrote under it." "$head"
# Not `reply`: seed-corpus.sh's corpus_comment and corpus_message take a local of that name, which a
# readonly variable refuses.
readonly person_reply="$corpus_item"
corpus_comment agent seed "$key" "The agent answers inside the person's thread." "$head"
readonly agent_reply="$corpus_item"
corpus_comment agent seed "$key" "Status: the agent is seeding the conversation."
readonly status_comment="$corpus_item"
corpus_comment person seed "$key" "$remark_words"
readonly remark="$corpus_item"
corpus_ask seed "$key" "Which store holds the conversation?"
readonly ask="$corpus_item"
corpus_answer seed "$ask" "Postgres, the database the pod runs." Postgres
pass seed "wrote the thread $head with the replies $person_reply and $agent_reply, the status $status_comment, the remark $remark and the ask $ask"

# The criterion the person wrote in that exchange, into the issue's tree under the thread's first
# block; the requirement under it; the decision an agent recorded; the check and the plan task.
tree_person_edit seed "$person_login writes a criterion into the tree of $key" "$tree" "$(jq -cn --arg p "$head_block" \
  '[{op: "insert", after: "end", markdown: (":::criterion{#k-discussion parent=\"" + $p + "\"}\nI want the conversation to hold what a person exchanged.\n:::")}]')"
readonly criterion_ref="dispatch://$key/artifact/$tree_slug#b-k-discussion"
tree_person_edit seed "$person_login writes a requirement under the criterion" "$spec" "$(jq -cn --arg p "$criterion_ref" --arg w "$requirement_words" \
  '[{op: "insert", after: "end", markdown: (":::requirement{#r-discussion parent=\"" + $p + "\"}\n" + $w + "\n:::")}]')"
# The decision holds the agent's reading of it, a node of kind reading.
corpus_edit seed "an agent records a decision that shaped the requirement, with its reading" "$spec" "$(jq -cn --arg s "dispatch://$key/spec#b-r-discussion" \
  '[{op: "insert", after: "end", markdown: ("::::decision{#d-discussion shaped=\"" + $s + "\"}\nThe conversation reads the comments, asks and messages a person wrote in.\n\n:::reading{#g-discussion}\nThe agent reads the decision as the person'"'"'s exchange asked it.\n:::\n::::")}]')"
cat >"$work/verification.md" <<VERIFICATION
How the conversation is proved.

:::check{#c-discussion serves="dispatch://$key/spec#b-r-discussion" surface="cli" run="bash e2e/acceptance/conversation-view.sh" when="before_merge" negative_control="Drop the remark and the read names it missing."}
The conversation answers every message a person exchanged.
:::
VERIFICATION
corpus_upload seed "$key" verification.md "$work/verification.md"
readonly verification="$corpus_doc"
corpus_edit seed "write version 2 of verification.md" "$verification" \
  "$(jq -cn '[{op: "replace", find: "How the conversation is proved.", with: "How the conversation is proved, in its second version."}]')"
cat >"$work/plan.md" <<PLAN
How the conversation is built.

:::plan_task{#t-discussion serves="dispatch://$key/spec#b-r-discussion"}
Walk the conversation from the issue.
:::
PLAN
corpus_upload seed "$key" plan.md "$work/plan.md"
readonly plan="$corpus_doc"
printf '# Notes\n\nThe first version of the notes.\n' >"$work/notes.md"
corpus_upload seed "$key" notes.md "$work/notes.md"
readonly notes="$corpus_doc"
corpus_edit seed "write version 2 of notes.md" "$notes" \
  "$(jq -cn '[{op: "replace", find: "The first version of the notes.", with: "The second version of the notes."}]')"
corpus_edit seed "write version 3 of notes.md" "$notes" \
  "$(jq -cn '[{op: "insert", after: "end", markdown: "A paragraph the third version adds."}]')"

# The agent's comment on the requirement's text, which no person answers.
tree_send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg a "$spec" --arg q "every message a person exchanged" \
  --argjson actor "$(tree_actor)" \
  '{body: "Does every message include the daemon'"'"'s?", anchor: {artifact: $a, quote: $q, block_id: "r-discussion"}, actor: $actor}')"
tree_want 201 seed "an agent comments on the requirement's text"
tree_field seed "an agent comments on the requirement's text" .id
readonly on_text="$tree_value"

# The daemon's own message, and the thread its session began with a person's and an agent's reply.
conversation_view_message "$daemon_session" seed "$key" "READY: the conversation's seed is written."
readonly daemon_message="$corpus_item"
conversation_view_message "$daemon_session" seed "$key" "The daemon begins a thread of its own."
readonly daemon_root="$corpus_item"
corpus_message person seed "$key" "The person answers inside the daemon's thread." "$daemon_root"
readonly daemon_person_reply="$corpus_item"
corpus_message agent seed "$key" "An agent answers inside the daemon's thread." "$daemon_root"
readonly daemon_agent_reply="$corpus_item"
pass seed "posted the daemon's message $daemon_message and its thread $daemon_root with the replies $daemon_person_reply and $daemon_agent_reply"
# A message the conversation holds, which the kind=message read must answer beside the daemon's it
# leaves out: the person's own message on the issue, and an agent's reply in the person's thread.
# Not `person_message`: seed-corpus.sh's corpus_large_issue takes a local of that name.
corpus_message person seed "$key" "The person writes a message of their own on the issue."
readonly own_message="$corpus_item"
corpus_message agent seed "$key" "An agent answers the person's message." "$own_message"
readonly own_message_reply="$corpus_item"
pass seed "posted the person's message $own_message and an agent's reply $own_message_reply in its thread"

corpus_wait_settled seed
# A result recorded on the check, a node of kind result.
corpus_result seed "$key" c-discussion pass "$corpus_commit_one" tester
readonly check_result="$tree_value"
corpus_review seed "$spec" approved
readonly approved_version="${corpus_approved[$spec]}"
cat >"$work/child.md" <<'CHILD'
The child's design.
CHILD
conversation_view_create seed "A child of the conversation's issue" "$work/child.md" "$key"
readonly child="$corpus_key"
pass seed "the person approved version $approved_version of the spec $spec; the child $child stands under $key"
# The seed carries every node kind this model holds, each read back from the store: the issue's
# documents and their versions, its blocks of each typed kind the seed writes and its prose, the
# comments and a comment's blocks, the ask, the messages, the result recorded on the check, and
# the child issue. Each kind is printed as `<kind>`, a block's as `block:<supertag>`. A comment's
# block (comment_block, `comment/<comment uuid>#<block id>`) has no graph_nodes row: the store
# holds it as its comment_blocks row (graphstore.KindCommentBlock).
readonly seeded_kinds='artifact ask block:block block:callout block:check block:criterion block:decision block:plan_task block:reading block:requirement comment comment_block issue message result version'
with_vars seed "read the node kinds the seed holds" "select coalesce(string_agg(distinct held, ' ' order by held), '') from (
    select case when n.kind = 'block' then 'block:' || n.supertag else n.kind end as held
    from graph_nodes n
    where (n.kind in ('block', 'artifact', 'version') and starts_with(n.id, :'key' || '/'))
       or (n.kind = 'issue' and n.id = :'child')
       or (n.kind in ('comment', 'ask', 'message', 'result') and n.id = any(string_to_array(:'items', ' ')))
    union all
    select 'comment_block' from comment_blocks b where starts_with(b.node_id, 'comment/' || :'head' || '#')
  ) kinds" key="$key" child="$child" head="$head" \
  items="$head $person_reply $agent_reply $status_comment $remark $on_text $ask $daemon_message $daemon_root $daemon_person_reply $daemon_agent_reply $own_message $own_message_reply $check_result"
kinds_missing=""
for wanted in $seeded_kinds; do
  [[ " $answer " == *" $wanted "* ]] || kinds_missing+=" $wanted"
done
[[ -z "$kinds_missing" ]] ||
  fail seed "the issue $key carries every node kind this model holds" "the store holds no node of${kinds_missing} among the seed's; it holds: $answer"
pass seed "the issue $key carries every node kind this model holds: $answer"

# --- what the conversation graph answers --------------------------------------------------------------

tree_section kinds
tree_keep kinds "/api/v1/issues/$key/tree" "$work/tree.json"
tree_keep kinds "/api/v1/issues/$key/artifacts/spec/text?blocks=true" "$work/spec-blocks.json"
for node in "comment:$head:the head of the person's thread" "comment:$person_reply:the person's reply" \
  "comment:$agent_reply:the agent's reply in the person's thread" "ask:$ask:the ask"; do
  IFS=: read -r kind id what <<<"$node"
  conversation_view_present kinds "$work/tree.json" "the read with no view answers $what as a node of the tree" "$kind" "$id" tree
done
# The result recorded on the check and the agent's reading of the decision, the two kinds the seed
# holds beside those the discussion names, are nodes of the tree too.
conversation_view_present kinds "$work/tree.json" "the read with no view answers the result recorded on the check as a node of the tree" \
  result "$check_result" tree
tree_kept_expect "$work/tree.json" kinds "the read with no view answers the agent's reading of the decision once, as a node of the tree" \
  '[["reading","tree"]]' '[.nodes[] | select(.block_id == "g-discussion") | [.supertag, .role]]'
# The remark nobody answered is plain prose, a node of its own.
held="$(jq -c --arg i "$remark" '[.nodes[] | select(.kind == "comment" and .id == $i) | .role]' "$work/tree.json")"
[[ "$held" == '["tree"]' ]] ||
  fail kinds "the remark nobody answered is a node of its own" "the prose node comment $remark is missing from the answer (it holds it as $held)"
pass kinds "the remark $remark nobody answered is a node of its own"
tree_kept_expect "$work/tree.json" kinds "the ask answers its answer on its own node" '"Postgres, the database the pod runs."' \
  --arg i "$ask" '[.nodes[] | select(.kind == "ask" and .id == $i) | .attributes.answer][0]'
tree_kept_expect "$work/tree.json" kinds \
  "the read answers the criterion, the requirement, the decision, the check and the plan task, each once, as nodes of the tree" \
  '[["c-discussion","check","tree"],["d-discussion","decision","tree"],["k-discussion","criterion","tree"],["r-discussion","requirement","tree"],["t-discussion","plan_task","tree"]]' \
  '[.nodes[] | select(.block_id == "k-discussion" or .block_id == "r-discussion" or .block_id == "d-discussion"
     or .block_id == "c-discussion" or .block_id == "t-discussion") | [.block_id, .supertag, .role]] | sort'
# Nothing of the workflow and nothing that is no part of the discussion: no artifact, no document
# version, no prose block of a role document. A role document's prose is each block the blocks read
# of the spec, verification.md or plan.md lists that is no typed block, a typed block's own
# paragraph among them. The room's own fragment is no role document: the paragraph of the criterion
# written into the tree is prose of that fragment, a node of the tree (R2, R16).
role_prose='[]'
for document in "$spec" "$verification" "$plan"; do
  tree_keep kinds "/api/v1/artifacts/$document/blocks" "$work/role-blocks.json"
  role_prose="$(jq -c --argjson typed "$typed_blocks" --argjson held "$role_prose" \
    '$held + [.[] | select((.type as $t | $typed | index($t)) == null) | .id]' "$work/role-blocks.json")"
done
[[ "$role_prose" != '[]' ]] ||
  fail kinds "read the prose blocks of the role documents of $key" "the blocks reads of $spec, $verification and $plan list none"
outside="$(jq -r --argjson prose "$role_prose" '.nodes[]
  | select(.kind == "artifact" or .kind == "version" or (.kind == "block" and (.block_id as $b | $prose | index($b)) != null))
  | "\(.kind) \(.id) (\(.supertag))"' "$work/tree.json")"
[[ -z "$outside" ]] || fail kinds "the read answers no artifact, no document version and no prose block of a role document" \
  "it answers ${outside//$'\n'/, }, nodes the graph must not hold"
pass kinds "the read answers no artifact, no document version and no prose block of a role document"
# The Summary paragraph's block, found by its words: the slice of the markdown its byte range names
# (the fixture is ASCII, so a slice by character is one by byte).
summary_block="$(jq -r --arg w "$summary_words" '.markdown as $m
  | [.blocks[]? | select(.type == "paragraph" and ($m[.from:.to] | sub("\\s+$"; "")) == $w) | .id] | first // empty' "$work/spec-blocks.json")"
[[ -n "$summary_block" ]] || fail kinds "find the spec's Summary paragraph among its blocks" "$(head -c 600 "$work/spec-blocks.json")"
tree_kept_expect "$work/tree.json" kinds "the read answers the spec's Summary paragraph nowhere" '[]' \
  --arg w "$summary_words" --arg b "$summary_block" '[.nodes[] | select((.text // "") == $w or (.text // "") == ($w + "\n") or (.block_id != null and .block_id == $b and $b != "")) | {kind, id}]'
conversation_view_newest kinds "$spec"
tree_send GET "/api/v1/artifacts/$spec/versions/$tree_value"
tree_want 200 kinds "read version $tree_value of the spec"
grep -qF -- "$summary_words" "$work/body" ||
  fail kinds "the spec's version still renders its Summary paragraph" "version $tree_value reads $(body)"
pass kinds "the spec's version $tree_value still renders its Summary paragraph"
# The Legion daemon's own message and every message of the thread it began are none of it.
conversation_view_absent kinds "$work/tree.json" "the read answers no message of the daemon's own session" message "$daemon_message" \
  "posted by $daemon_session"
for message in "$daemon_root" "$daemon_person_reply" "$daemon_agent_reply"; do
  conversation_view_absent kinds "$work/tree.json" "the read answers no message of the thread $daemon_session began ($message)" \
    message "$message" "its root $daemon_root, posted by $daemon_session"
done
# The person's own message and the agent's reply in its thread are of it.
conversation_view_present kinds "$work/tree.json" "the read with no view answers the person's own message as a node of the tree" \
  message "$own_message" tree
conversation_view_present kinds "$work/tree.json" "the read with no view answers the agent's reply in the person's thread of messages" \
  message "$own_message_reply" tree
tree_send GET "/api/v1/issues/$key/tree?kind=message"
tree_want 200 kinds "read the messages of the tree of $key (kind=message)"
cp "$work/body" "$work/tree-messages.json"
for message in "$daemon_message" "$daemon_root" "$daemon_person_reply" "$daemon_agent_reply"; do
  conversation_view_absent kinds "$work/tree-messages.json" "the read with kind=message answers no message of the daemon or its thread ($message)" \
    message "$message" "its root $daemon_root, posted by $daemon_session"
done
# The same read answers the messages of the conversation, so each absence above is the daemon's
# rule's and not the filter's: a kind=message read that answered no message would fail here.
conversation_view_present kinds "$work/tree-messages.json" "the read with kind=message answers the person's own message" \
  message "$own_message" tree
conversation_view_present kinds "$work/tree-messages.json" "the read with kind=message answers the agent's reply in the person's thread of messages" \
  message "$own_message_reply" tree
tree_keep kinds "/api/v1/issues/$key/messages" "$work/messages.json"
tree_kept_expect "$work/messages.json" kinds "the issue's messages hold the daemon's message, its thread's root and both replies" \
  '[true,true,true,true]' --arg a "$daemon_message" --arg b "$daemon_root" --arg c "$daemon_person_reply" --arg d "$daemon_agent_reply" \
  '[.. | objects | select(has("body") and has("author")) | .id] as $ids | [$a, $b, $c, $d] | map(. as $m | $ids | index($m) != null)'
# The agent's status comment is no node; its reply in the person's thread is; the comments hold both.
conversation_view_absent kinds "$work/tree.json" "the read answers no status comment of an agent at the top of the issue" comment "$status_comment"
tree_keep kinds "/api/v1/issues/$key/comments" "$work/comments.json"
tree_kept_expect "$work/comments.json" kinds "the issue's comments hold the agent's status comment and its reply in the person's thread" \
  '[true,true]' --arg a "$status_comment" --arg b "$agent_reply" \
  '[.. | objects | select(has("body") and has("author")) | .id] as $ids | [$a, $b] | map(. as $m | $ids | index($m) != null)'
# The agent's comment on the requirement's text, which no person answered: context under the
# requirement, and no node under it.
held="$(jq -c --arg i "$on_text" '[.nodes[] | select(.kind == "comment" and .id == $i) | {role, parent: .parent.id, under: .parent.supertag}]' "$work/tree.json")"
requirement_node="$(jq -r '[.nodes[] | select(.block_id == "r-discussion") | .id][0] // empty' "$work/tree.json")"
[[ "$held" == "$(jq -cn --arg p "$requirement_node" '[{role: "context", parent: $p, under: "requirement"}]')" ]] ||
  fail kinds "the agent's comment on the requirement's text is answered under the requirement as context" \
    "the answer holds the comment $on_text as $held, want it once as context under $requirement_node"
pass kinds "the agent's comment $on_text on the requirement's text is answered under the requirement as context"
# The comment's own blocks, its text, stand with it (R8: "An item's own blocks stand with their
# item"; "Under a conversation item, the item's own blocks come first"); no other node hangs under
# it, since no node hangs under a comment on text but its replies, and nobody replied.
tree_kept_expect "$work/tree.json" kinds "no node of the read but the comment's own blocks stands under the agent's comment on the text" '[]' \
  --arg i "$on_text" '[.nodes[] | select(.parent != null and .parent.kind == "comment" and .parent.id == $i)
    | select((.kind == "comment_block" and (.id | startswith("comment/" + $i + "#"))) | not) | {kind, id}]'
tree_kept_expect "$work/tree.json" kinds "the read answers the memo callout first, before any conversation node" \
  '["memo","callout"]' '[.nodes[] | select(.role != "context" and .parent != null)][0] | [.block_id, .supertag]'

# --- the memo and the spec query ---------------------------------------------------------------

tree_section memo
tree_keep memo "/api/v1/issues/$key/tree?view=spec" "$work/spec-view.json"
tree_kept_expect "$work/spec-view.json" memo "?view=spec answers the memo callout first, as a node of the query" \
  '["memo","callout","query"]' '.nodes[0] | [.block_id, .supertag, .role]'
second="$(jq -c '[.nodes[] | select(.block_id == "second") | {kind, id, role}]' "$work/spec-view.json")"
[[ "$second" == "[]" ]] || fail memo "?view=spec answers no callout but the memo" "it answers the second callout second: $second"
pass memo "?view=spec answers no callout but the memo"
conversation_view_digest memo "$spec" "$approved_version"
readonly approved_digest="$tree_value"
corpus_edit memo "change one of the memo's three sentences" "$spec" \
  "$(jq -cn --arg f "$memo_sentence" --arg w "$memo_changed" '[{op: "replace", find: $f, with: $w}]')"
conversation_view_newest memo "$spec"
readonly memo_version="$tree_value"
conversation_view_digest memo "$spec" "$memo_version"
readonly memo_digest="$tree_value"
[[ "$memo_digest" != "$approved_digest" ]] ||
  fail memo "a change to one of the memo's sentences moves the digest the spec approval recorded" \
    "version $memo_version reads the digest $memo_digest, the one version $approved_version, which the person approved, reads"
pass memo "a change to one of the memo's sentences moves the digest from $approved_digest (version $approved_version) to $memo_digest (version $memo_version)"
corpus_edit memo "change the second callout's words" "$spec" \
  "$(jq -cn --arg f "$second_words" --arg w "$second_changed" '[{op: "replace", find: $f, with: $w}]')"
conversation_view_newest memo "$spec"
readonly second_version="$tree_value"
[[ "$second_version" != "$memo_version" ]] || fail memo "the edit of the second callout writes a version" "the newest is still $memo_version"
conversation_view_digest memo "$spec" "$second_version"
[[ "$tree_value" == "$memo_digest" ]] ||
  fail memo "an edit of the second callout moves no spec digest" "version $second_version reads $tree_value, version $memo_version $memo_digest"
pass memo "an edit of the second callout moves no spec digest: version $second_version reads $tree_value as version $memo_version did"
cat >"$work/third.md" <<'THIRD'
A paragraph that is the issue's first child.

:::callout{#late kind="note" title=""}
A callout after the paragraph, which is no memo.
:::
THIRD
conversation_view_create memo "An issue whose first child is a paragraph" "$work/third.md"
readonly third="$corpus_key"
tree_keep memo "/api/v1/issues/$third/tree?view=spec" "$work/third-spec.json"
tree_kept_expect "$work/third-spec.json" memo "?view=spec of $third, whose first child is a paragraph, answers no callout" '[]' \
  '[.nodes[] | select(.supertag == "callout") | {kind, id, block_id}]'

# --- the largest issue ---------------------------------------------------------------------------

tree_section large
# The comments of the large issue. The largest issue R8 measures holds corpus_measured_comments,
# fewer than one comment page of the tree read (comment_page_size), so a seed of exactly that many
# leaves comments_before null on the first page and never reads an older page. corpus_large_issue
# writes a person's thread head for each third comment and an agent's reply in that thread for the
# next, both of which the conversation takes, and an agent's own comment for the third, which it
# does not: large_comments of them give the conversation more than two pages of comments, so the
# read follows comments_before more than once. Every other count is the measured one.
readonly comment_page_size=200
readonly large_comments=$((corpus_measured_comments > 630 ? corpus_measured_comments : 630))
printf 'the large issue holds the measured counts the pull request body records: %s blocks over %s documents, %s messages and %s asks (measured %s), and %s comments, the measured %s raised past two comment pages of %s\n' \
  "$corpus_measured_blocks" "$corpus_measured_documents" "$corpus_measured_messages" "$corpus_measured_asks" \
  "$corpus_measured_at" "$large_comments" "$corpus_measured_comments" "$comment_page_size"
corpus_large_issue large "$corpus_measured_blocks" "$corpus_measured_documents" "$large_comments" \
  "$corpus_measured_messages" "$corpus_measured_asks"
readonly large="$corpus_key"
keys+=("$large")
# corpus_large_issue writes the spec, verification.md and plan.md, the three role documents, first,
# and every other document of the issue after them. Where the measured issue holds no other
# document, the seed still holds one other document of 1,000 blocks, so the blocks read below runs.
readonly large_other_documents=$((corpus_measured_documents > 3 ? corpus_measured_documents - 3 : 0))
if ((large_other_documents == 0)); then
  corpus_filler other-1000.md 1000 >"$work/large-other.md"
  corpus_upload large "$large" other-1000.md "$work/large-other.md"
  corpus_wait_settled large
fi
readonly large_others_held=$((large_other_documents > 0 ? large_other_documents : 1))
# The skeleton, page by page through its node cursor, its comment cursor never changed.
cursor="" pages=0 nodes=0 largest_page=0 largest_bytes=0
: >"$work/large-comments.txt"
: >"$work/large-documents.tsv"
: >"$work/large-page-1.txt"
: >"$work/large-first-messages.txt"
: >"$work/large-first-blocks.tsv"
while true; do
  tree_read agent "$large" "" "" "$cursor"
  tree_want 200 large "read page $((pages + 1)) of the tree of $large"
  pages=$((pages + 1))
  bytes="$(wc -c <"$work/body")"
  bytes="${bytes//[[:space:]]/}"
  ((bytes <= page_bytes_bound)) ||
    fail large "each page of the read with no view holds at most $page_bytes_bound uncompressed bytes" "page $pages holds $bytes"
  count="$(jq '.nodes | length' "$work/body")"
  nodes=$((nodes + count))
  ((count <= largest_page)) || largest_page="$count"
  ((bytes <= largest_bytes)) || largest_bytes="$bytes"
  jq -r '.nodes[] | select(.kind == "comment") | .id' "$work/body" >>"$work/large-page-1.txt"
  jq -r '.nodes[] | select(.kind == "message") | .id' "$work/body" >>"$work/large-first-messages.txt"
  jq -r '.nodes[] | select(.kind == "block") | [.id, (.block_id // "")] | @tsv' "$work/body" >>"$work/large-first-blocks.tsv"
  jq -r '.document_nodes[]? | [.artifact_id, (.block_count | tostring), .slug] | @tsv' "$work/body" >>"$work/large-documents.tsv"
  if ((pages == 1)); then
    cp "$work/body" "$work/large-first.json"
  fi
  cursor="$(jq -r '.cursor // empty' "$work/body")"
  [[ -n "$cursor" ]] || break
done
cat "$work/large-page-1.txt" >>"$work/large-comments.txt"
pass large "the read with no view of $large ends after $pages pages of $nodes nodes, each at most $page_bytes_bound bytes"
# The skeleton holds the typed blocks of the three role documents and no block of another
# document, which document_nodes lists instead (R8).
: >"$work/large-role-typed.txt"
for slug in spec verification-md plan-md; do
  tree_keep large "/api/v1/issues/$large/artifacts/$slug/text?blocks=true" "$work/large-role.json"
  jq -r --argjson typed "$typed_blocks" '.blocks[]? | select((.type as $t | $typed | index($t)) != null) | .id' \
    "$work/large-role.json" >>"$work/large-role-typed.txt"
done
[[ -s "$work/large-role-typed.txt" ]] || fail large "read the typed blocks of the role documents of $large" "the spec, verification.md and plan.md list none"
cut -f2 "$work/large-first-blocks.tsv" | LC_ALL=C sort -u >"$work/large-first-block-ids.txt"
typed_missing="$(LC_ALL=C sort -u "$work/large-role-typed.txt" | LC_ALL=C comm -23 - "$work/large-first-block-ids.txt" | tr '\n' ' ')"
[[ -z "$typed_missing" ]] ||
  fail large "the read with no view answers the skeleton: each typed block of the spec, verification.md and plan.md" "it answers none of the blocks $typed_missing"
pass large "the read with no view answers each of the $(LC_ALL=C sort -u "$work/large-role-typed.txt" | wc -l | tr -d ' ') typed blocks of the role documents of $large"
others="$(cut -f1 "$work/large-first-blocks.tsv" | { grep -v "^$large/tree#" || true; } | tr '\n' ' ')"
[[ -z "$others" ]] ||
  fail large "the read with no view answers no block of another document of $large, which document_nodes lists" "it answers the blocks $others"
pass large "the read with no view answers no block of another document of $large"
# The skeleton holds its conversation: every message the conversation takes, each a person wrote
# and each of a thread a person wrote in.
tree_keep large "/api/v1/issues/$large/messages" "$work/large-message-list.json"
jq -r '[.. | objects | select(has("body") and has("author") and has("in_reply_to"))] as $all
  | ([$all[] | {key: .id, value: .}] | from_entries) as $by
  | def root($m): if ($m.in_reply_to // null) == null or ($by[$m.in_reply_to] // null) == null then $m.id else root($by[$m.in_reply_to]) end;
  ([$all[] | {root: root(.), person: (.author.kind == "user")}] | group_by(.root) | map(select(any(.[]; .person)) | .[0].root)) as $threads
  | $all[] | select(root(.) as $r | $threads | index($r) != null) | .id' "$work/large-message-list.json" |
  LC_ALL=C sort >"$work/large-messages-taken.txt"
[[ -s "$work/large-messages-taken.txt" ]] || fail large "read the messages of $large the conversation takes" "the issue's messages hold none a person wrote in"
LC_ALL=C sort "$work/large-first-messages.txt" >"$work/large-first-messages-sorted.txt"
cmp -s "$work/large-messages-taken.txt" "$work/large-first-messages-sorted.txt" ||
  fail large "the read with no view answers the skeleton's conversation: each message of $large a person wrote in, once" \
    "only in the issue's messages: $(LC_ALL=C comm -23 "$work/large-messages-taken.txt" "$work/large-first-messages-sorted.txt" | tr '\n' ' '); in the read: $(tr '\n' ' ' <"$work/large-first-messages.txt")"
pass large "the read with no view answers each of the $(wc -l <"$work/large-messages-taken.txt" | tr -d ' ') messages of $large a person wrote in, once"
# The comments the walk takes: each a person wrote, and each of a thread a person wrote in, in the
# order of the comment pages, newest first by when each was written and then by id (R8).
tree_keep large "/api/v1/issues/$large/comments" "$work/large-comment-list.json"
jq -r '[.. | objects | select(has("body") and has("author") and has("reply_to"))] as $all
  | ([$all[] | {key: .id, value: .}] | from_entries) as $by
  | def root($c): if ($c.reply_to // null) == null or ($by[$c.reply_to] // null) == null then $c.id else root($by[$c.reply_to]) end;
  ([$all[] | {root: root(.), person: (.author.kind == "user")}] | group_by(.root) | map(select(any(.[]; .person)) | .[0].root)) as $threads
  | $all[] | select(root(.) as $r | $threads | index($r) != null) | .id' "$work/large-comment-list.json" |
  LC_ALL=C sort >"$work/large-taken.txt"
taken_count="$(wc -l <"$work/large-taken.txt")"
taken_count="${taken_count//[[:space:]]/}"
((taken_count > 2 * comment_page_size)) ||
  fail large "the conversation of $large holds more than two pages of $comment_page_size comments, so the read follows comments_before more than once" \
    "it holds $taken_count of the $large_comments comments"
with_vars large "order the comments of $large the walk takes, newest first" \
  "select string_agg(id::text, ' ' order by created_at desc, id desc) from comments
   where id = any(array_remove(string_to_array(:'taken', ' '), '')::uuid[])" taken="$(tr '\n' ' ' <"$work/large-taken.txt")"
tr ' ' '\n' <<<"$answer" >"$work/large-ordered.txt"
[[ "$(wc -l <"$work/large-ordered.txt" | tr -d ' ')" == "$taken_count" ]] ||
  fail large "order the comments of $large the walk takes, newest first" "the comments table orders $(wc -l <"$work/large-ordered.txt" | tr -d ' ') of $taken_count"
readonly comment_pages_wanted=$(((taken_count + comment_page_size - 1) / comment_page_size))
# The first read answers the newest 200 comments; each comments_before page the next older 200.
comment_pages=1
conversation_view_comment_page large 1 "$work/large-page-1.txt"
before="$(jq -r '.comments_before // empty' "$work/large-first.json")"
while [[ -n "$before" ]]; do
  tree_send GET "/api/v1/issues/$large/tree?comments_before=$(q "$before")"
  tree_want 200 large "read the comments of $large before $before"
  comment_pages=$((comment_pages + 1))
  bytes="$(wc -c <"$work/body")"
  bytes="${bytes//[[:space:]]/}"
  ((bytes <= page_bytes_bound)) ||
    fail large "each comments_before page holds at most $page_bytes_bound uncompressed bytes" "comment page $comment_pages holds $bytes"
  jq -r '.nodes[] | select(.kind == "comment") | .id' "$work/body" >"$work/large-page-$comment_pages.txt"
  cat "$work/large-page-$comment_pages.txt" >>"$work/large-comments.txt"
  conversation_view_comment_page large "$comment_pages" "$work/large-page-$comment_pages.txt"
  before="$(jq -r '.comments_before // empty' "$work/body")"
done
((comment_pages == comment_pages_wanted)) ||
  fail large "the read follows comments_before, one page at a time, until it is null" \
    "it read $comment_pages comment pages of the $taken_count comments the walk takes, want $comment_pages_wanted pages of $comment_page_size"
pass large "the read follows comments_before through $((comment_pages - 1)) older pages until it is null"
LC_ALL=C sort "$work/large-comments.txt" >"$work/large-read.txt"
[[ "$(uniq -d "$work/large-read.txt" | wc -l)" -eq 0 ]] ||
  fail large "the pages hold each comment once" "repeated: $(uniq -d "$work/large-read.txt" | tr '\n' ' ')"
cmp -s "$work/large-taken.txt" "$work/large-read.txt" ||
  fail large "the pages hold each of the issue's comments the walk takes once" \
    "the issue holds $(wc -l <"$work/large-taken.txt") such comments and the pages $(wc -l <"$work/large-read.txt"): only in the list: $(LC_ALL=C comm -23 "$work/large-taken.txt" "$work/large-read.txt" | tr '\n' ' '); only in the pages: $(LC_ALL=C comm -13 "$work/large-taken.txt" "$work/large-read.txt" | tr '\n' ' ')"
pass large "the $comment_pages comment pages hold each of the $taken_count comments the walk takes once"
# The blocks of each document document_nodes lists, through the blocks read. document_nodes lists
# each document of the issue, the role documents among them; every other document, whose blocks
# the skeleton leaves out, must be among them (R8).
listed=0 others_listed=0 blocks_read=0
while IFS=$'\t' read -r document block_count slug; do
  tree_send GET "/api/v1/artifacts/$document/blocks"
  tree_want 200 large "read the blocks of the document $document of $large"
  count="$(jq 'length' "$work/body")"
  [[ "$count" == "$block_count" ]] ||
    fail large "the blocks read of each document lists as many blocks as its block_count" \
      "the document $document ($slug) lists $count blocks, and document_nodes answers block_count $block_count"
  listed=$((listed + 1))
  [[ "$slug" == spec || "$slug" == verification-md || "$slug" == plan-md ]] || others_listed=$((others_listed + 1))
  blocks_read=$((blocks_read + count))
done < <(LC_ALL=C sort -u "$work/large-documents.tsv")
((others_listed == large_others_held)) ||
  fail large "the read with no view lists each other document of $large in document_nodes" \
    "it lists $listed documents, $others_listed of them other than the spec, verification.md and plan.md, want $large_others_held"
pass large "the blocks read of each of the $listed documents document_nodes lists, the $others_listed other documents among them, answers its block_count"
printf 'large issue %s: page size %s nodes (largest page), %s bytes at most, %s node pages, %s nodes, %s comment pages, %s blocks read through the blocks read\n' \
  "$large" "$largest_page" "$largest_bytes" "$pages" "$nodes" "$comment_pages" "$blocks_read"

# --- the child's ask -------------------------------------------------------------------------------

tree_section child
corpus_ask child "$child" "Which store holds the child's conversation?"
readonly child_ask="$corpus_item"
tree_keep child "/api/v1/issues/$child/tree" "$work/child-tree.json"
conversation_view_present child "$work/child-tree.json" "the child's ask is a conversation node of the child's tree" ask "$child_ask" tree
tree_kept_expect "$work/child-tree.json" child "the child's tree answers no artifact and no document version" '[]' \
  '[.nodes[] | select(.kind == "artifact" or .kind == "version") | {kind, id}]'

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
