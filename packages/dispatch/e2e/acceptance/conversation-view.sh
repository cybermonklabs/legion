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
#               requirement; a check and a plan task that serve it; an agent's comment on the
#               requirement's text that no person answered; the spec's memo callout as its first
#               child, its Summary paragraph, and a second callout; two documents of several
#               versions each (verification.md and notes.md); a message the Legion daemon's session
#               legion-daemon:<PROJECT> posts; a message thread that session began, with a reply the
#               person wrote and one an agent wrote; the person's approval of the spec; and a child
#               issue.
#   kinds       the read with no view answers, as nodes, the comments, the replies, the agent's reply
#               in the person's thread, the ask with its answer held on its node, the remark, the
#               criterion, the requirement, the decision, the check and the plan task; it answers no
#               artifact, no document version and no prose block of a role document, the Summary
#               among them, which the spec's version still renders; no message of the daemon's own
#               session and no message of its thread, with no view and with kind=message, while the
#               issue's messages hold all four; not the agent's status comment, while the issue's
#               comments hold it and the reply; the agent's comment on the requirement's text under
#               the requirement as context, with no node under it; and the memo callout first.
#   memo        ?view=spec answers the memo first as a node of the query and not the second callout;
#               a change to one of the memo's three sentences moves the digest the spec approval
#               recorded, and an edit of the second callout moves no digest; a third issue whose
#               first child is a paragraph and which holds a callout after it answers no callout on
#               ?view=spec.
#   large       a second issue of the size of the largest of the ten issues R8 measures, which
#               seed-corpus.sh's corpus_large_issue writes with its corpus_measured_* constants, the
#               counts the pull request body records: its read with no view is followed by its node
#               cursor to the end, each page at most 2,000,000 uncompressed bytes, answering the
#               skeleton with the newest 200 comments; its older comments are read through
#               comments_before until null, and the pages hold each comment the walk takes once; and
#               each document document_nodes lists is read through GET /api/v1/artifacts/{id}/blocks,
#               which lists as many blocks as its block_count. It prints the page size, the node
#               count, the number of comment pages and the number of blocks it read.
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
require_commands cmp comm curl jq psql sort tr uniq wc
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
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
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
readonly key="$corpus_key" spec="$corpus_spec"
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
readonly reply="$corpus_item"
corpus_comment agent seed "$key" "The agent answers inside the person's thread." "$head"
readonly agent_reply="$corpus_item"
corpus_comment agent seed "$key" "Status: the agent is seeding the conversation."
readonly status_comment="$corpus_item"
corpus_comment person seed "$key" "$remark_words"
readonly remark="$corpus_item"
corpus_ask seed "$key" "Which store holds the conversation?"
readonly ask="$corpus_item"
corpus_answer seed "$ask" "Postgres, the database the pod runs." Postgres
pass seed "wrote the thread $head with the replies $reply and $agent_reply, the status $status_comment, the remark $remark and the ask $ask"

# The criterion the person wrote in that exchange, into the issue's tree under the thread's first
# block; the requirement under it; the decision an agent recorded; the check and the plan task.
tree_person_edit seed "$person_login writes a criterion into the tree of $key" "$tree" "$(jq -cn --arg p "$head_block" \
  '[{op: "insert", after: "end", markdown: (":::criterion{#k-discussion parent=\"" + $p + "\"}\nI want the conversation to hold what a person exchanged.\n:::")}]')"
readonly criterion_ref="dispatch://$key/artifact/$tree_slug#b-k-discussion"
tree_person_edit seed "$person_login writes a requirement under the criterion" "$spec" "$(jq -cn --arg p "$criterion_ref" --arg w "$requirement_words" \
  '[{op: "insert", after: "end", markdown: (":::requirement{#r-discussion parent=\"" + $p + "\"}\n" + $w + "\n:::")}]')"
corpus_edit seed "an agent records a decision that shaped the requirement" "$spec" "$(jq -cn --arg s "dispatch://$key/spec#b-r-discussion" \
  '[{op: "insert", after: "end", markdown: (":::decision{#d-discussion shaped=\"" + $s + "\"}\nThe conversation reads the comments, asks and messages a person wrote in.\n:::")}]')"
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

corpus_wait_settled seed
corpus_review seed "$spec" approved
readonly approved_version="${corpus_approved[$spec]}"
cat >"$work/child.md" <<'CHILD'
The child's design.
CHILD
conversation_view_create seed "A child of the conversation's issue" "$work/child.md" "$key"
readonly child="$corpus_key"
pass seed "the person approved version $approved_version of the spec $spec; the child $child stands under $key"

# --- what the conversation graph answers --------------------------------------------------------------

tree_section kinds
tree_keep kinds "/api/v1/issues/$key/tree" "$work/tree.json"
tree_keep kinds "/api/v1/issues/$key/artifacts/spec/text?blocks=true" "$work/spec-blocks.json"
for node in "comment:$head:the head of the person's thread" "comment:$reply:the person's reply" \
  "comment:$agent_reply:the agent's reply in the person's thread" "ask:$ask:the ask"; do
  IFS=: read -r kind id what <<<"$node"
  conversation_view_present kinds "$work/tree.json" "the read with no view answers $what as a node of the tree" "$kind" "$id" tree
done
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
# version, no prose block of a role document.
outside="$(jq -r --argjson typed "$typed_blocks" '.nodes[]
  | select(.kind == "artifact" or .kind == "version" or (.kind == "block" and (.supertag as $s | $typed | index($s)) == null))
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
tree_send GET "/api/v1/issues/$key/tree?kind=message"
tree_want 200 kinds "read the messages of the tree of $key (kind=message)"
cp "$work/body" "$work/tree-messages.json"
for message in "$daemon_message" "$daemon_root" "$daemon_person_reply" "$daemon_agent_reply"; do
  conversation_view_absent kinds "$work/tree-messages.json" "the read with kind=message answers no message of the daemon or its thread ($message)" \
    message "$message" "its root $daemon_root, posted by $daemon_session"
done
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
tree_kept_expect "$work/tree.json" kinds "no node of the read stands under the agent's comment on the text" '[]' \
  --arg i "$on_text" '[.nodes[] | select(.parent != null and .parent.kind == "comment" and .parent.id == $i) | {kind, id}]'
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
printf 'the large issue holds the measured counts the pull request body records: %s blocks over %s documents, %s comments, %s messages and %s asks (measured %s)\n' \
  "$corpus_measured_blocks" "$corpus_measured_documents" "$corpus_measured_comments" "$corpus_measured_messages" \
  "$corpus_measured_asks" "$corpus_measured_at"
corpus_large_issue large "$corpus_measured_blocks" "$corpus_measured_documents" "$corpus_measured_comments" \
  "$corpus_measured_messages" "$corpus_measured_asks"
readonly large="$corpus_key"
keys+=("$large")
# The skeleton, page by page through its node cursor, its comment cursor never changed.
cursor="" pages=0 nodes=0 largest_page=0 largest_bytes=0
: >"$work/large-comments.txt"
: >"$work/large-documents.tsv"
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
  jq -r '.nodes[] | select(.kind == "comment") | .id' "$work/body" >>"$work/large-comments.txt"
  jq -r '.document_nodes[]? | [.artifact_id, (.block_count | tostring)] | @tsv' "$work/body" >>"$work/large-documents.tsv"
  if ((pages == 1)); then
    cp "$work/body" "$work/large-first.json"
  fi
  cursor="$(jq -r '.cursor // empty' "$work/body")"
  [[ -n "$cursor" ]] || break
done
pass large "the read with no view of $large ends after $pages pages of $nodes nodes, each at most $page_bytes_bound bytes"
tree_kept_expect "$work/large-first.json" large "the first page answers at most 200 comments, the newest" 'true' \
  '[.nodes[] | select(.kind == "comment")] | length <= 200'
# The older comments, one comments_before page at a time, until it is null.
comment_pages=1
before="$(jq -r '.comments_before // empty' "$work/large-first.json")"
while [[ -n "$before" ]]; do
  tree_send GET "/api/v1/issues/$large/tree?comments_before=$(q "$before")"
  tree_want 200 large "read the comments of $large before $before"
  comment_pages=$((comment_pages + 1))
  jq -r '.nodes[] | select(.kind == "comment") | .id' "$work/body" >>"$work/large-comments.txt"
  before="$(jq -r '.comments_before // empty' "$work/body")"
done
tree_keep large "/api/v1/issues/$large/comments" "$work/large-comment-list.json"
# The comments the walk takes: each a person wrote, and each of a thread a person wrote in.
jq -r '[.. | objects | select(has("body") and has("author") and has("reply_to"))] as $all
  | ([$all[] | {key: .id, value: .}] | from_entries) as $by
  | def root($c): if ($c.reply_to // null) == null or ($by[$c.reply_to] // null) == null then $c.id else root($by[$c.reply_to]) end;
  ([$all[] | {root: root(.), person: (.author.kind == "user")}] | group_by(.root) | map(select(any(.[]; .person)) | .[0].root)) as $threads
  | $all[] | select(root(.) as $r | $threads | index($r) != null) | .id' "$work/large-comment-list.json" |
  LC_ALL=C sort >"$work/large-taken.txt"
LC_ALL=C sort "$work/large-comments.txt" >"$work/large-read.txt"
[[ "$(uniq -d "$work/large-read.txt" | wc -l)" -eq 0 ]] ||
  fail large "the pages hold each comment once" "repeated: $(uniq -d "$work/large-read.txt" | tr '\n' ' ')"
cmp -s "$work/large-taken.txt" "$work/large-read.txt" ||
  fail large "the pages hold each of the issue's comments the walk takes once" \
    "the issue holds $(wc -l <"$work/large-taken.txt") such comments and the pages $(wc -l <"$work/large-read.txt"): only in the list: $(LC_ALL=C comm -23 "$work/large-taken.txt" "$work/large-read.txt" | tr '\n' ' '); only in the pages: $(LC_ALL=C comm -13 "$work/large-taken.txt" "$work/large-read.txt" | tr '\n' ' ')"
pass large "the $comment_pages comment pages hold each of the $(wc -l <"$work/large-taken.txt" | tr -d ' ') comments the walk takes once"
# The blocks of each document document_nodes lists, through the blocks read.
listed=0 blocks_read=0
while IFS=$'\t' read -r document block_count; do
  tree_send GET "/api/v1/artifacts/$document/blocks"
  tree_want 200 large "read the blocks of the document $document of $large"
  count="$(jq 'length' "$work/body")"
  [[ "$count" == "$block_count" ]] ||
    fail large "the blocks read of each document lists as many blocks as its block_count" \
      "the document $document lists $count blocks, and document_nodes answers block_count $block_count"
  listed=$((listed + 1))
  blocks_read=$((blocks_read + count))
done < <(LC_ALL=C sort -u "$work/large-documents.tsv")
((listed > 0)) || fail large "the read with no view lists the documents of $large in document_nodes" "it lists none"
pass large "the blocks read of each of the $listed documents document_nodes lists answers its block_count"
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
