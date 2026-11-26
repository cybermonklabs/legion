#!/usr/bin/env bash
# The seeded corpus of task TT1 of the issue-tree plan: a project of issues written through
# the API of the release before this branch, the server lib-baseline.sh boots from BASELINE_SHA,
# so that every row of it is one that release's own writers left. What reads it:
#   - R19's two counts (c-migration-size's body and the first lines of c-old-versions-render): the
#     artifact_versions rows with a markdown body and the standing approvals, each printed here
#     with the time it was read, as the spec's two queries read them;
#   - T2b's and T6's sizing, and T19a's `envoy-dispatch convert-issues --project <PROJECT>
#     --dry-run` timing (c-room-cutover's dry-run reading), which run over the database this
#     leaves;
#   - T16a's page size (c-tree-page) and c-conversation-holds-the-discussion's large issue: the
#     largest issue here has the shape of the largest of the ten issues R8 measures, the memory
#     experiment's measurement, read through Dispatch's API at 2026-11-16T09:11Z: 10,842 blocks
#     over 243 documents, 34 comments, 34 messages and no ask, 10,910 items. Those are the
#     constants below.
#
# The corpus, in project PROJECT (each issue's key is printed as it is written):
#   The shapes issue. Its spec, its verification.md and its plan.md, and a notes.md, each hold the
#   shapes c-tree-census lists - headings followed by paragraphs, a nested list, a list whose last
#   item is empty, a typed block (a callout) holding a code block and a table, and a quote holding
#   a list - beside requirements, a decision and an ask block in the spec, checks that serve them
#   (one serves three) in verification.md, and plan tasks that serve them and need each other in
#   plan.md. Each document holds three versions, written by an edit and by an upload under the
#   same name. A person approved the spec, verification.md and notes.md at their newest version,
#   which still stand, and plan.md at its second, which its third supersedes. It holds one image
#   upload, whose version holds no markdown; comments that cite blocks of each document, among them
#   a person's thread with an agent's reply to one of its blocks and the person's reply; an agent's
#   status comment; messages of an agent and of a person, with a reply; an ask a person answered
#   and an ask left open; and results recorded on three checks, a fail among them.
#   The child issue, under the shapes issue: its three documents hold the shapes in two versions
#   each, its check serves a requirement of its parent and one of its own, its plan task serves its
#   parent's, a result is recorded on that check, and a person approved its spec.
#   The boundary issues, for R19's counts and the dry run, the first four with a spec,
#   verification.md and plan.md holding the shapes in two versions each: one approved at its
#   newest versions, one whose approval a later version supersedes and whose verification.md a
#   person asked changes of, one approved and then asked changes of, and one closed (`done`, which
#   the conversion passes over); and a fifth holding a spec alone and blocked by the first.
#   The large issue, of CORPUS_LARGE_DOCUMENTS documents holding CORPUS_LARGE_BLOCKS blocks (as the
#   blocks read of each document's newest version counts them), CORPUS_LARGE_COMMENTS comments
#   (every one citing a block), CORPUS_LARGE_MESSAGES messages and CORPUS_LARGE_ASKS asks: its spec,
#   verification.md, plan.md and notes-NNN.md documents, each of two versions and each holding the
#   shapes, with standing approvals of the spec, verification.md, plan.md and every tenth notes
#   document.
#   The role documents of LARGEST_ISSUE, read from Dispatch with the pod's token. On a separate
#   test issue, each reference to the source issue names the test issue instead. Pointer attributes
#   with targets outside the three copied documents are omitted and counted. No source text is
#   printed. The conversion dry run measures this issue by its printed test key.
# What the earlier release's API cannot write is not here: c-old-versions-render's block written
# author-only. That release writes every block at the visibility `project` (refs/block_nodes.go
# at BASELINE_SHA), no route takes a visibility, and a `visibility` attribute on a typed block is
# refused as `400 INVALID_MARKDOWN: ... typed block "callout" does not declare attribute
# "visibility"`.
#
# Inputs: DATABASE_URL (an empty database on this machine: this script refuses one that holds a
# table, since the corpus must be the database's only content, and keeps it when it ends, with the
# earlier release's schema and the corpus in it); PROJECT (the key of the project it creates there:
# CORE, as the checks name it); DEV_SIGNIN_LOGIN (the person, `alice`, signed in through the dev
# sign-in before the project's create names them as its default assignee, who writes every
# approval and every person's comment and message); DISPATCH_TOKEN (optional: the agent bearer the
# earlier release's server accepts; unset, baseline_start makes one); lib-baseline.sh's
# BASELINE_SHA, BASELINE_BINARY, BASELINE_EXTRA_ENV and LEGION_WORKSPACE, as its head comment states
# them; and the large issue's size, CORPUS_LARGE_BLOCKS, CORPUS_LARGE_DOCUMENTS,
# CORPUS_LARGE_COMMENTS, CORPUS_LARGE_MESSAGES and CORPUS_LARGE_ASKS, each defaulting to the
# measured size above, so a quick run can ask for a smaller one. Run from packages/dispatch:
#   DATABASE_URL=… PROJECT=CORE DEV_SIGNIN_LOGIN=alice DISPATCH_TOKEN=e2e-token \
#     LARGEST_ISSUE=ACME-1 BASELINE_SHA=<commit> bash e2e/acceptance/seed-corpus.sh
# DISPATCH_URL and DISPATCH_TOKEN_FILE are the pod's deployment URL and bearer file, for GET only.
#
# It prints every issue, document and item it wrote, `count <name>=<value> at <time>` for each
# count it wrote and read, one `PASS: <section> <what>` per assertion, and ends with
# `PASS: seed-corpus.sh complete <N> sections` once its last section ran; the first failing
# assertion prints `FAIL: <section> <what>: <observed>` and the script exits 1.
#
# Sourced instead, after lib.sh and lib-tree.sh, it gives its writers to a driver that writes the
# same corpus through another server - corpus_large_issue, for a driver that must hold an issue of
# the measured size - and the measured constants corpus_measured_*, and it boots nothing and writes
# nothing itself. The writers read what lib-tree.sh's requests read (`base`, `work`, `session`,
# `project`, the header file tree_init wrote and the person tree_sign_in signed in), and
# `database_url`, which their wait for each document's settlement reads.

set -euo pipefail
set +x

# The largest of the ten issues R8 measures, the memory experiment's measurement, as Dispatch's API
# read it at 2026-11-16T09:11Z: its blocks, documents, comments, messages and asks.
readonly corpus_measured_blocks=10842
readonly corpus_measured_documents=243
readonly corpus_measured_comments=34
readonly corpus_measured_messages=34
readonly corpus_measured_asks=0
readonly corpus_measured_at=2026-11-16T09:11Z

# The commits the results name. The project maps no repository, so the record route asks no
# repository whether it holds them and stores each as given.
readonly corpus_commit_one=1111111111111111111111111111111111111111
readonly corpus_commit_two=2222222222222222222222222222222222222222

# A 1x1 PNG, whose bytes the upload route stores as an image: its version holds no markdown.
readonly corpus_png='iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='

# What the writers leave their caller: the last issue's key and spec id, the last document's id and
# slug, the last item's id, the block count corpus_blocks read and the first paragraph it found,
# the blocks corpus_large_issue wrote, the version each approval named, by document id, and the
# sentence corpus_revise last wrote into each document it revised, by document id, which its next
# revision finds.
corpus_key=""
corpus_spec=""
corpus_doc=""
corpus_slug=""
corpus_item=""
corpus_block_count=0
corpus_paragraph=""
corpus_large_written=0
declare -gA corpus_approved=()
declare -gA corpus_sentence=()

# corpus_now: the time a count is printed with, to the second, UTC.
corpus_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# corpus_count NAME VALUE [AT]: prints one count with the time it was taken (AT, else now).
corpus_count() { printf 'count %s=%s at %s\n' "$1" "$2" "${3:-$(corpus_now)}"; }

# corpus_shapes NAME PREFIX: the shapes c-tree-census lists, as one markdown section of 23 blocks
# whose first paragraph names NAME, so each document's copy is its own and an edit can find it,
# and whose typed block's id is PREFIX-typed, one id of its document.
corpus_shapes() {
  local name="$1" prefix="$2"
  printf '## The shapes of %s\n\n' "$name"
  printf 'A paragraph under the heading of %s.\n\n' "$name"
  printf 'A second paragraph under that heading.\n\n'
  printf -- '- An item of a nested list\n  - A nested item\n    - A deeper item\n- A second item of the nested list\n\n'
  printf 'A paragraph between the two lists.\n\n'
  printf -- '- The first item of a list whose last item is empty\n- \n\n'
  printf ':::callout{#%s-typed kind="note" title="A typed block holding code and a table"}\n' "$prefix"
  printf 'A paragraph in the typed block.\n\n'
  # shellcheck disable=SC2016 # the backticks are the markdown's code fence, not a substitution
  printf '```sh\necho "code in a typed block"\n```\n\n'
  printf '| Column | Value |\n| --- | --- |\n| first | 1 |\n| second | 2 |\n:::\n\n'
  printf '> A quote that holds a list:\n>\n> - the first quoted item\n> - the second quoted item\n\n'
}

# corpus_filler NAME COUNT: COUNT blocks more, a heading before each seven paragraphs, each block
# one of its own: a heading is one block and so is a paragraph.
corpus_filler() {
  local name="$1" count="$2" index
  for ((index = 1; index <= count; index++)); do
    if ((index % 8 == 1)); then
      printf '### Part %d of %s\n\n' "$(((index + 7) / 8))" "$name"
    else
      printf 'Paragraph %d of %s holds the words a document of a large issue holds.\n\n' "$index" "$name"
    fi
  done
}

# corpus_issue SECTION TITLE FILE [PARENT [BLOCKED_BY]]: creates an issue of `project` as the agent,
# FILE's markdown its spec, under PARENT and blocked by BLOCKED_BY where given; sets corpus_key and
# corpus_spec. Each create is forced past the duplicate check (`force`), since every issue of the
# corpus holds the same shapes and the check takes the second for a copy of the first.
corpus_issue() {
  local section="$1" title="$2" file="$3" parent="${4:-}" blocked="${5:-}"
  jq -cn --arg p "$project" --arg t "$title" --rawfile s "$file" --arg parent "$parent" \
    --arg blocked "$blocked" --argjson a "$(tree_actor)" \
    '{project: $p, title: $t, spec: $s, force: true, actor: $a}
     + (if $parent == "" then {} else {parent: $parent} end)
     + (if $blocked == "" then {} else {blocked_by: [$blocked]} end)' >"$work/issue.json"
  tree_send POST /api/v1/issues "@$work/issue.json"
  tree_want 201 "$section" "create the issue \"$title\""
  tree_field "$section" "create the issue \"$title\"" .key
  corpus_key="$tree_value"
  tree_field "$section" "create the issue \"$title\"" .primary_artifact_id
  corpus_spec="$tree_value"
  printf 'wrote issue %s "%s" spec=%s%s%s\n' "$corpus_key" "$title" "$corpus_spec" \
    "${parent:+ parent=$parent}" "${blocked:+ blocked_by=$blocked}"
}

# corpus_upload SECTION ISSUE NAME FILE: uploads FILE as the document NAME of ISSUE, as the agent.
# A name ISSUE holds already writes that document's next version, and that upload names the token
# of the version it replaces, which the earlier release refuses an upload without; the first
# version names none. Sets corpus_doc and corpus_slug.
corpus_upload() {
  local section="$1" issue="$2" name="$3" file="$4" token=""
  tree_send GET "/api/v1/issues/$issue/artifacts/${name//./-}/text"
  case "$status" in
    200) tree_field "$section" "read the token of $name on $issue" .token && token="$tree_value" ;;
    404) ;;
    *) fail "$section" "read the token of $name on $issue" "$tree_last_request answered HTTP $status: $(body)" ;;
  esac
  jq -cn --arg n "$name" --rawfile c "$file" --arg t "$token" --argjson a "$(tree_actor)" \
    '{name: $n, content: $c, actor: $a} + (if $t == "" then {} else {precondition: {document: $t}} end)' \
    >"$work/upload.json"
  tree_send POST "/api/v1/issues/$issue/artifacts" "@$work/upload.json"
  tree_want 201 "$section" "upload $name to $issue"
  tree_field "$section" "upload $name to $issue" .artifact.id
  corpus_doc="$tree_value"
  tree_field "$section" "upload $name to $issue" .artifact.slug
  corpus_slug="$tree_value"
}

# corpus_edit SECTION WHAT DOCUMENT OPS: applies the edit ops OPS (a JSON list) to DOCUMENT as the
# agent, which writes its next version; an edit that changed nothing fails.
corpus_edit() {
  tree_send POST "/api/v1/artifacts/$3/edits" \
    "$(jq -cn --argjson ops "$4" --argjson a "$(tree_actor)" '{ops: $ops, actor: $a}')"
  tree_want 200 "$1" "$2"
  jq -e '.changed == true' "$work/body" >/dev/null ||
    fail "$1" "$2" "the edit changed nothing: $(body)"
}

# corpus_revise SECTION DOCUMENT NAME VERSION: the edit that writes a version of a document of
# the corpus: the first paragraph of its shapes, which names NAME, says which version wrote it. It
# finds that paragraph as the last revision of DOCUMENT left it, or as corpus_shapes wrote it.
corpus_revise() {
  local section="$1" document="$2" name="$3" version="$4" from to
  from="${corpus_sentence[$document]:-A paragraph under the heading of $name.}"
  to="A paragraph under the heading of $name, as version $version writes it."
  corpus_edit "$section" "write version $version of $name" "$document" \
    "$(jq -cn --arg f "$from" --arg w "$to" '[{op: "replace", find: $f, with: $w}]')"
  corpus_sentence["$document"]="$to"
}

# corpus_version SECTION DOCUMENT: the number of DOCUMENT's newest version, in tree_value.
corpus_version() {
  tree_send GET "/api/v1/artifacts/$2/text"
  tree_want 200 "$1" "read the newest version of $2"
  tree_field "$1" "read the newest version of $2" .version
}

# corpus_blocks SECTION DOCUMENT: the blocks of DOCUMENT's newest version as its blocks read
# answers them, every block a nested one included: their count in corpus_block_count and the id of
# the first paragraph in corpus_paragraph, which a comment cites.
corpus_blocks() {
  tree_send GET "/api/v1/artifacts/$2/blocks"
  tree_want 200 "$1" "read the blocks of $2"
  corpus_block_count="$(jq -er 'if type == "array" then length else error end' "$work/body" 2>/dev/null)" ||
    fail "$1" "read the blocks of $2" "no list of blocks: $(body)"
  corpus_paragraph="$(jq -r '[.[] | select(.type == "paragraph") | .id][0] // ""' "$work/body")"
  [[ -n "$corpus_paragraph" ]] || fail "$1" "read the blocks of $2" "no paragraph among them: $(body)"
}

# corpus_review SECTION DOCUMENT STATE [REASON]: the person reviews DOCUMENT's newest settled
# version; an approval is kept in corpus_approved by the version it names, a request for changes
# takes the document out of it.
corpus_review() {
  local section="$1" document="$2" state="$3" reason="${4:-}"
  tree_person_send POST "/api/v1/artifacts/$document/reviews" \
    "$(jq -cn --arg s "$state" --arg r "$reason" '{state: $s} + (if $r == "" then {} else {reason: $r} end)')"
  tree_want 201 "$section" "$tree_person reviews $document ($state)"
  tree_field "$section" "$tree_person reviews $document" .version
  if [[ "$state" == approved ]]; then
    corpus_approved["$document"]="$tree_value"
  else
    unset 'corpus_approved[$document]'
  fi
  printf 'wrote review %s of %s at version %s\n' "$state" "$document" "$tree_value"
}

# corpus_comment AS SECTION ISSUE BODY [REPLY_TO [BLOCK]]: a comment on ISSUE as AS (agent or
# person), a reply to REPLY_TO, and to its block BLOCK, where given; sets corpus_item.
corpus_comment() {
  local as="$1" section="$2" issue="$3" text="$4" reply="${5:-}" block="${6:-}" actor='null'
  [[ "$as" != agent ]] || actor="$(tree_actor)"
  tree_request "$as" POST "/api/v1/issues/$issue/comments" "$(jq -cn --arg b "$text" --arg r "$reply" \
    --arg k "$block" --argjson a "$actor" \
    '{body: $b} + (if $a == null then {} else {actor: $a} end)
     + (if $r == "" then {} else {reply_to: $r} end) + (if $k == "" then {} else {reply_to_block: $k} end)')"
  tree_want 201 "$section" "$as comments on $issue"
  tree_field "$section" "$as comments on $issue" .id
  corpus_item="$tree_value"
}

# corpus_message AS SECTION ISSUE BODY [IN_REPLY_TO]: a message on ISSUE as AS, a reply to
# IN_REPLY_TO where given; sets corpus_item.
corpus_message() {
  local as="$1" section="$2" issue="$3" text="$4" reply="${5:-}" actor='null'
  [[ "$as" != agent ]] || actor="$(tree_actor)"
  tree_request "$as" POST "/api/v1/issues/$issue/messages" "$(jq -cn --arg b "$text" --arg r "$reply" \
    --argjson a "$actor" \
    '{body: $b} + (if $a == null then {} else {actor: $a} end) + (if $r == "" then {} else {in_reply_to: $r} end)')"
  tree_want 201 "$section" "$as posts a message on $issue"
  tree_field "$section" "$as posts a message on $issue" .id
  corpus_item="$tree_value"
}

# corpus_ask SECTION ISSUE QUESTION: the agent opens an ask on ISSUE with two options; sets
# corpus_item. corpus_answer SECTION ASK TEXT OPTION: the person answers it.
corpus_ask() {
  tree_send POST "/api/v1/issues/$2/asks" "$(jq -cn --arg q "$3" --argjson a "$(tree_actor)" \
    '{question: $q, options: [{label: "Postgres", description: "The database the pod runs."},
      {label: "Files", description: "A directory beside the server."}], actor: $a}')"
  tree_want 201 "$1" "open an ask on $2"
  tree_field "$1" "open an ask on $2" .id
  corpus_item="$tree_value"
}
corpus_answer() {
  tree_person_send POST "/api/v1/asks/$2/answer" "$(jq -cn --arg t "$3" --arg o "$4" \
    '{text: $t, selected: [$o], expected_edited_at: null}')"
  tree_want 200 "$1" "$tree_person answers ask $2"
}

# corpus_result SECTION ISSUE CHECK VERDICT COMMIT ROLE: records a result of the check block CHECK
# on ISSUE as the agent; a fail counts one failed case, as the route requires of it.
corpus_result() {
  local failed=0
  [[ "$4" != fail ]] || failed=1
  tree_send POST "/api/v1/issues/$2/results" "$(jq -cn --arg c "$3" --arg v "$4" --arg m "$5" \
    --arg r "$6" --argjson f "$failed" --argjson a "$(tree_actor)" \
    '{check: $c, role: $r, verdict: $v, commit: $m, failed: $f, surface: "cli",
      run: "bash e2e/acceptance/seed-corpus.sh", observed: ("the seeded " + $v),
      negative_control: "Break the shape and the check fails.", actor: $a}')"
  tree_want 201 "$1" "record $4 of $3 on $2"
  tree_field "$1" "record $4 of $3 on $2" .result.id
  printf 'wrote result %s %s of %s on %s\n' "$tree_value" "$4" "$3" "$2"
}

# corpus_image SECTION ISSUE NAME: uploads a PNG as the image NAME of ISSUE, as the agent, through
# the multipart form the dashboard sends; sets corpus_doc.
corpus_image() {
  printf '%s' "$corpus_png" | base64 -d >"$work/image.png" ||
    fail "$1" "make the image $3" "base64 could not decode it"
  tree_last_request="POST /api/v1/issues/$2/artifacts (multipart $3)"
  status="$(curl -sS -X POST -o "$work/body" -w '%{http_code}' -H "@$work/auth" \
    -F "name=$3" -F "actor=$(tree_actor)" -F "file=@$work/image.png;type=image/png" \
    "$base/api/v1/issues/$2/artifacts" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  tree_want 201 "$1" "upload the image $3 to $2"
  expect "$1" "$3 is stored as an image" '"image"' .artifact.kind
  tree_field "$1" "upload the image $3 to $2" .artifact.id
  corpus_doc="$tree_value"
}

# corpus_wait_settled SECTION: waits until no document owes a settlement (doc_settlements_pending)
# and every document version is indexed (no document_block_versions row whose version is null),
# read through `database_url`: a review records the newest settled version, so one written while a
# version is still settling would name the version before it, and a count of document_blocks taken
# before the index has run counts fewer blocks than the documents hold.
corpus_wait_settled() {
  local deadline=$((SECONDS + tree_wait_seconds)) at owed unindexed
  while true; do
    query "$1" "read what the documents still owe" "select clock_timestamp(),
      (select count(*) from doc_settlements_pending),
      (select count(*) from document_block_versions where version is null)"
    IFS='|' read -r at owed unindexed <<<"$answer"
    [[ "$owed" != 0 || "$unindexed" != 0 ]] || return 0
    ((SECONDS < deadline)) ||
      fail "$1" "every document settles and is indexed within ${tree_wait_seconds} s" \
        "at $at $owed documents still owe a settlement and $unindexed an index"
    sleep "$tree_poll_seconds"
  done
}

# corpus_approvals_stand SECTION: each approval corpus_approved holds names its document's newest
# version still: no version landed after the one it approved.
corpus_approvals_stand() {
  local document
  for document in "${!corpus_approved[@]}"; do
    corpus_version "$1" "$document"
    [[ "$tree_value" == "${corpus_approved[$document]}" ]] ||
      fail "$1" "the approval of $document stands" \
        "it names version ${corpus_approved[$document]}, and the newest is $tree_value"
  done
  pass "$1" "${#corpus_approved[@]} approvals stand at their document's newest version"
}

# corpus_large_document SECTION ISSUE NAME DOCUMENT: the second version of one document of the
# large issue and its block count, added to corpus_large_written.
corpus_large_document() {
  local section="$1" issue="$2" name="$3" document="$4"
  corpus_revise "$section" "$document" "$name" 2
  corpus_blocks "$section" "$document"
  corpus_large_written=$((corpus_large_written + corpus_block_count))
  pass "$section" "wrote $issue $name (2 versions, $corpus_block_count blocks)"
}

# corpus_large_issue SECTION BLOCKS DOCUMENTS COMMENTS MESSAGES ASKS: writes the large issue - an
# issue of DOCUMENTS documents holding BLOCKS blocks in all, as each document's blocks read of its
# newest version counts them, with COMMENTS comments, MESSAGES messages and ASKS asks - against
# `base`, as the agent `session` and the person tree_sign_in signed in, in `project`. Its spec,
# verification.md and plan.md come first, then notes-NNN.md documents; each holds the shapes and
# requirements, checks or tasks where it is a role document, and as many blocks more as bring the
# issue to BLOCKS: each document takes its share of the blocks still to write, and once the first
# notes document has shown how many blocks a notes document holds without them, every later one is
# written to exactly its share, the last to what is left. A share smaller than a document's own
# shapes leaves the issue over BLOCKS, never under. Every comment cites a block: a third of them
# open a person's thread, a third are an agent's reply to the second block of the last such thread,
# and a third are an agent's own; a third of the messages are a person's, a third an agent's reply
# to the last of them, and a third an agent's own; half the
# asks are answered. A person approves the spec, verification.md, plan.md and every tenth notes
# document at their newest version. It sets corpus_key to the issue and prints its counts.
corpus_large_issue() {
  local section="$1" blocks="$2" documents="$3" comments="$4" messages="$5" asks="$6"
  local key index name share filler notes_base="" left_docs left_blocks requirements=5 cited
  local -a citable=() approve=()
  local root="" person_message=""
  corpus_large_written=0

  # share: the blocks the next document holds, the ceiling of what is left over the documents left.
  # Sets the caller's share.
  large_share() {
    left_docs=$((documents - index))
    left_blocks=$((blocks - corpus_large_written))
    if ((left_docs <= 1)); then
      share="$left_blocks"
    else
      share=$(((left_blocks + left_docs - 1) / left_docs))
    fi
    ((share >= 0)) || share=0
  }

  index=0
  large_share
  # The three role documents' own blocks are not known before they are read, so each takes its
  # share less an estimate of what its shapes and typed blocks hold; the notes documents after them
  # make up the difference, since their own count is read off the first of them.
  filler=$((share - 23 - 4 - 2 * requirements))
  ((filler >= 0)) || filler=0
  {
    printf ':::callout{#memo kind="note" title=""}\nThe large issue holds as many blocks as the largest issue measured. Its documents hold the shapes the census reads. Its comments cite their blocks.\n:::\n\n'
    printf 'The summary paragraph of the large issue.\n\n## Requirements\n\n'
    for ((cited = 1; cited <= requirements; cited++)); do
      printf ':::requirement{#r-large-%d}\nThe large issue meets its requirement %d.\n:::\n\n' "$cited" "$cited"
    done
    corpus_shapes spec large-spec
    corpus_filler spec "$filler"
  } >"$work/large-spec.md"
  corpus_issue "$section" "The largest issue, of $blocks blocks over $documents documents" "$work/large-spec.md"
  key="$corpus_key"
  corpus_large_document "$section" "$key" spec "$corpus_spec"
  approve+=("$corpus_spec")
  citable+=("dispatch://$key/spec#b-r-large-1" "dispatch://$key/spec#b-$corpus_paragraph")

  if ((documents >= 2)); then
    index=1
    large_share
    filler=$((share - 23 - 1 - 2 * requirements))
    ((filler >= 0)) || filler=0
    {
      printf 'How the large issue is proved.\n\n'
      for ((cited = 1; cited <= requirements; cited++)); do
        printf ':::check{#c-large-%d serves="dispatch://%s/spec#b-r-large-%d" surface="cli" run="bash e2e/acceptance/seed-corpus.sh" negative_control="Leave requirement %d out and the check fails."}\nRequirement %d of the large issue is proved.\n:::\n\n' \
          "$cited" "$key" "$cited" "$cited" "$cited"
      done
      corpus_shapes verification.md large-verification
      corpus_filler verification.md "$filler"
    } >"$work/large-verification.md"
    corpus_upload "$section" "$key" verification.md "$work/large-verification.md"
    corpus_large_document "$section" "$key" verification.md "$corpus_doc"
    approve+=("$corpus_doc")
    citable+=("dispatch://$key/artifact/verification-md#b-c-large-1")
  fi

  if ((documents >= 3)); then
    index=2
    large_share
    filler=$((share - 23 - 1 - 2 * requirements))
    ((filler >= 0)) || filler=0
    {
      printf 'How the large issue is built.\n\n'
      printf ':::plan_task{#t-large-1 serves="dispatch://%s/spec#b-r-large-1"}\nWrite the first part of the large issue.\n:::\n\n' "$key"
      for ((cited = 2; cited <= requirements; cited++)); do
        printf ':::plan_task{#t-large-%d serves="dispatch://%s/spec#b-r-large-%d" needs="dispatch://%s/artifact/plan-md#b-t-large-%d"}\nWrite part %d of the large issue.\n:::\n\n' \
          "$cited" "$key" "$cited" "$key" "$((cited - 1))" "$cited"
      done
      corpus_shapes plan.md large-plan
      corpus_filler plan.md "$filler"
    } >"$work/large-plan.md"
    corpus_upload "$section" "$key" plan.md "$work/large-plan.md"
    corpus_large_document "$section" "$key" plan.md "$corpus_doc"
    approve+=("$corpus_doc")
    citable+=("dispatch://$key/artifact/plan-md#b-t-large-1")
  fi

  for ((index = 3; index < documents; index++)); do
    name="$(printf 'notes-%03d.md' "$((index - 2))")"
    large_share
    if [[ -z "$notes_base" ]]; then
      filler=$((share - 25))
    else
      filler=$((share - notes_base))
    fi
    ((filler >= 0)) || filler=0
    {
      printf '# %s\n\nThis document of the large issue cites dispatch://%s/spec#b-r-large-%d.\n\n' \
        "$name" "$key" "$(((index % requirements) + 1))"
      corpus_shapes "$name" "notes-$((index - 2))"
      corpus_filler "$name" "$filler"
    } >"$work/large-notes.md"
    corpus_upload "$section" "$key" "$name" "$work/large-notes.md"
    corpus_large_document "$section" "$key" "$name" "$corpus_doc"
    [[ -n "$notes_base" ]] || notes_base=$((corpus_block_count - filler))
    citable+=("dispatch://$key/artifact/$corpus_slug#b-$corpus_paragraph")
    (((index - 2) % 10 != 1)) || approve+=("$corpus_doc")
  done

  for ((index = 1; index <= comments; index++)); do
    cited="${citable[$(((index - 1) % ${#citable[@]}))]}"
    case $((index % 3)) in
      1)
        corpus_comment person "$section" "$key" "Comment $index: which part does $cited hold?"$'\n\n'"Its second block asks for the version."
        root="$corpus_item"
        ;;
      2)
        if [[ -n "$root" ]]; then
          corpus_comment agent "$section" "$key" "Comment $index answers the second block: $cited holds it." "$root" b2
        else
          corpus_comment agent "$section" "$key" "Comment $index: $cited holds it."
        fi
        ;;
      0) corpus_comment agent "$section" "$key" "Comment $index: the census reads $cited." ;;
    esac
  done
  for ((index = 1; index <= messages; index++)); do
    if ((index % 3 == 1)); then
      corpus_message person "$section" "$key" "Message $index: is the large issue written?"
      person_message="$corpus_item"
    elif ((index % 3 == 2)) && [[ -n "$person_message" ]]; then
      corpus_message agent "$section" "$key" "Message $index: it is, block by block." "$person_message"
    else
      corpus_message agent "$section" "$key" "Message $index: the agent reports the large issue's progress."
    fi
  done
  for ((index = 1; index <= asks; index++)); do
    corpus_ask "$section" "$key" "Question $index of the large issue: which store holds it?"
    ((index % 2 == 0)) || corpus_answer "$section" "$corpus_item" "Postgres, for question $index." Postgres
  done

  corpus_wait_settled "$section"
  for cited in "${approve[@]}"; do
    corpus_review "$section" "$cited" approved
  done

  ((corpus_large_written >= blocks)) ||
    fail "$section" "the large issue holds at least $blocks blocks" "its documents hold $corpus_large_written"
  pass "$section" "the large issue $key holds $corpus_large_written blocks, $blocks asked for"
  corpus_key="$key"
  local at=""
  at="$(corpus_now)"
  corpus_count large_issue "$key" "$at"
  corpus_count large_documents "$documents" "$at"
  corpus_count large_blocks "$corpus_large_written" "$at"
  corpus_count large_comments "$comments" "$at"
  corpus_count large_messages "$messages" "$at"
  corpus_count large_asks "$asks" "$at"
  corpus_count large_items "$((corpus_large_written + comments + messages + asks))" "$at"
  corpus_count large_approvals "${#approve[@]}" "$at"
}

# Sourced, this file ends here: a driver that sourced lib.sh and lib-tree.sh has the writers.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  declare -F tree_request >/dev/null || {
    printf 'seed-corpus.sh: source lib.sh and lib-tree.sh before this file\n' >&2
    return 2
  }
  return 0
fi

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<empty database>?sslmode=disable \
         PROJECT=CORE DEV_SIGNIN_LOGIN=alice LARGEST_ISSUE=<issue-key> [DISPATCH_TOKEN=e2e-token] \
         [BASELINE_SHA=<commit> | BASELINE_BINARY=<path>] [BASELINE_EXTRA_ENV=$'NAME=value\n...'] \
         [CORPUS_LARGE_BLOCKS=10842] [CORPUS_LARGE_DOCUMENTS=243] [CORPUS_LARGE_COMMENTS=34] \
         [CORPUS_LARGE_MESSAGES=34] [CORPUS_LARGE_ASKS=0] bash e2e/acceptance/seed-corpus.sh
       (run from packages/dispatch; set the pod's DISPATCH_URL and DISPATCH_TOKEN_FILE for the
       source reads. DATABASE_URL names an empty loopback database for the earlier release.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"

require_env LARGEST_ISSUE DATABASE_URL PROJECT DEV_SIGNIN_LOGIN DISPATCH_URL DISPATCH_TOKEN_FILE
require_commands base64 cmp curl date jq psql python3 sed
[[ "$LARGEST_ISSUE" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || tree_refuse "LARGEST_ISSUE must be an issue key"
[[ -r "$DISPATCH_TOKEN_FILE" ]] || tree_refuse "DISPATCH_TOKEN_FILE is not readable"
readonly source_issue="$LARGEST_ISSUE" source_url="${DISPATCH_URL%/}"
[[ "$source_url" == https://* || "$source_url" == http://* ]] ||
  tree_refuse "DISPATCH_URL must be an HTTP(S) URL"
# shellcheck disable=SC2153 # PROJECT and DATABASE_URL are the caller's inputs, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="$DEV_SIGNIN_LOGIN"
loopback_database_url "$database_url" ||
  tree_refuse "DATABASE_URL must name one loopback test database with no connection override"

# Read the deployment only with GET. Neither a source response nor a copied document's write
# refusal can be printed: either may contain the source issue's words or a private reference.
corpus_source_get() {
  local what="$1" path="$2" file="$3" code
  code="$(curl -q -s --max-time 120 --proto '=http,https' -H "@$work/source-auth" \
    -o "$file" -w '%{http_code}' "$source_url$path")" ||
    fail largest "read the source $what" "transport failed"
  [[ "$code" == 200 ]] || fail largest "read the source $what" "HTTP $code"
}

corpus_read_largest() {
  local bearer role field id
  bearer="$(<"$DISPATCH_TOKEN_FILE")"
  bearer="${bearer#"${bearer%%[![:space:]]*}"}"
  bearer="${bearer%"${bearer##*[![:space:]]}"}"
  [[ -n "$bearer" && "$bearer" != *[[:space:]]* ]] ||
    tree_refuse "DISPATCH_TOKEN_FILE must hold one bearer"
  printf 'Authorization: Bearer %s\n' "$bearer" >"$work/source-auth"
  unset bearer
  corpus_source_get issue "/api/v1/issues/$source_issue" "$work/source-issue.json"
  corpus_source_get "block schema" /api/v1/schema/blocks "$work/source-schema.json"
  for role in spec verification-md plan-md; do
    case "$role" in
      spec) field=primary_artifact_id ;;
      verification-md) field=verification_artifact_id ;;
      plan-md) field=plan_artifact_id ;;
    esac
    id="$(jq -er --arg f "$field" '.[$f] | strings | select(test("^[a-f0-9-]{36}$"))' \
      "$work/source-issue.json" 2>/dev/null)" ||
      fail largest "find the source $role" "the issue read has no artifact id for this role"
    corpus_source_get "$role markdown" "/api/v1/artifacts/$id/text" "$work/source-text.json"
    jq -ej '.markdown | strings' "$work/source-text.json" >"$work/$role.source.md" 2>/dev/null ||
      fail largest "read the source $role" "the text read has no markdown string"
  done
  rm -f "$work/source-auth" "$work/source-issue.json" "$work/source-text.json"
}

# Strip a whole pointer attribute if one of its targets will not be present in the copy.
# An attribute is one Yjs item even when its value contains multiple pointers. Bootstrap the
# three documents without forward pointers and restore those pointers after their targets exist.
corpus_rewrite_largest() {
  if ! python3 - "$work" "$source_issue" "$1" 2>"$work/rewrite-error" <<'PY'
import json
from pathlib import Path
import re
import sys

work, source, seeded = sys.argv[1:]
work = Path(work)
schema = json.loads((work / "source-schema.json").read_text(encoding="utf-8"))
pointers = {
    block["name"]: {
        name for name, attr in block.get("attributes", {}).items()
        if attr["kind"] in ("block[]", "block", "ask")
    } for block in schema["types"]
}
if not {"serves", "needs"} <= pointers.get("plan_task", set()):
    raise ValueError("missing plan pointer attributes")

roles = ("spec", "verification-md", "plan-md")
self_ref = re.compile(r"dispatch://" + re.escape(source) + r"(?![A-Za-z0-9-])")
block_ref = re.compile(
    rf"dispatch://{re.escape(source)}/(?:artifact/)?({'|'.join(roles)})#b-\S+"
)
directive = re.compile(r"(?P<open>[ \t>]*:{3,}(?P<type>\w+)\{)(?P<attrs>.*)\}(?P<end>[ \t]*)$")
attribute = re.compile(r'[ \t]+(?P<name>[a-z_][a-z0-9_-]*)=(?P<value>"(?:\\.|[^"\\])*"|[^ \t"]+)')
code_fence = re.compile(r"(`{3,}|~{3,})(.*)")

def rewrite(markdown, available):
    lines, drops, fence = [], 0, None
    for line in markdown.split("\n"):
        code = code_fence.fullmatch(line.lstrip(" \t>"))
        if fence is not None:
            if code and code[1].startswith(fence) and not code[2].strip():
                fence = None
        elif code:
            fence = code[1]
        else:
            opener = directive.fullmatch(line)
            if opener:
                names = pointers.get(opener["type"], set())

                def keep(match):
                    nonlocal drops
                    if match["name"] not in names:
                        return match[0]
                    values = match["value"].strip('"').split()
                    targets = [block_ref.fullmatch(value) for value in values]
                    if all(target and target[1] in available for target in targets):
                        return match[0]
                    drops += 1
                    return ""

                attrs = attribute.sub(keep, opener["attrs"])
                line = opener["open"] + attrs + "}" + opener["end"]
        lines.append(self_ref.sub(f"dispatch://{seeded}", line))
    return "\n".join(lines), drops

dropped = 0
for index, role in enumerate(roles):
    source_text = (work / f"{role}.source.md").read_text(encoding="utf-8")
    final, drops = rewrite(source_text, roles)
    bootstrap, _ = rewrite(source_text, roles[:index + 1])
    dropped += drops
    (work / f"{role}.bootstrap.md").write_text(bootstrap, encoding="utf-8")
    (work / f"{role}.copy.md").write_text(final, encoding="utf-8")
print(dropped)
PY
  then
    fail largest "rewrite the copied role documents" "could not read their canonical markdown or pointer schema"
  fi
}

# Unlike the synthetic fixture writer, this writer never includes a response body in a failure.
corpus_upload_largest_role() {
  local issue="$1" role="$2" name="$3" file="$4" version_token=""
  tree_send GET "/api/v1/issues/$issue/artifacts/$role/text"
  case "$status" in
    200) version_token="$(jq -er '.token | strings | select(length > 0)' "$work/body" 2>/dev/null)" ||
      fail largest "read the current token of $role on $issue" "no token" ;;
    404) ;;
    *) fail largest "read the current token of $role on $issue" "HTTP $status" ;;
  esac
  jq -cn --arg n "$name" --rawfile c "$file" --arg t "$version_token" --argjson a "$(tree_actor)" \
    '{name: $n, content: $c, actor: $a} + (if $t == "" then {} else {precondition: {document: $t}} end)' \
    >"$work/largest-upload.json" 2>/dev/null ||
    fail largest "prepare the upload of $role on $issue" "could not encode its markdown"
  tree_send POST "/api/v1/issues/$issue/artifacts" "@$work/largest-upload.json"
  [[ "$status" == 201 ]] || fail largest "upload $role on $issue" "HTTP $status (body withheld)"
  jq -e --arg slug "$role" '.artifact.slug == $slug' "$work/body" >/dev/null 2>&1 ||
    fail largest "the upload writes $role on $issue" "unexpected artifact slug (body withheld)"
}

# The large issue's size: the measured one, unless the run asks for another.
large_blocks="${CORPUS_LARGE_BLOCKS:-$corpus_measured_blocks}"
large_documents="${CORPUS_LARGE_DOCUMENTS:-$corpus_measured_documents}"
large_comments="${CORPUS_LARGE_COMMENTS:-$corpus_measured_comments}"
large_messages="${CORPUS_LARGE_MESSAGES:-$corpus_measured_messages}"
large_asks="${CORPUS_LARGE_ASKS:-$corpus_measured_asks}"
for size in large_blocks large_documents large_comments large_messages large_asks; do
  [[ "${!size}" =~ ^[0-9]+$ ]] || {
    printf 'seed-corpus.sh: CORPUS_%s is %s, not a whole number\n' "${size^^}" "${!size}" >&2
    usage
    exit 2
  }
done
((large_documents >= 3)) || {
  printf 'seed-corpus.sh: CORPUS_LARGE_DOCUMENTS is %s: the large issue holds its spec, verification.md and plan.md at least\n' "$large_documents" >&2
  exit 2
}
readonly large_blocks large_documents large_comments large_messages large_asks

work_dir
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The corpus is the database's only content, so every count below is the corpus's own: a database
# holding a table is refused before the earlier release's server migrates it.
query setup "read whether the database is empty" \
  "select count(*) from pg_tables where schemaname not in ('pg_catalog', 'information_schema')"
[[ "$answer" == 0 ]] || {
  printf 'seed-corpus.sh: the database DATABASE_URL names holds %s tables; the corpus is written to an empty database\n' "$answer" >&2
  exit 2
}

section=setup
started="$(corpus_now)"
printf 'seed-corpus.sh started at %s\n' "$started"
corpus_count measured_largest_issue_blocks "$corpus_measured_blocks" "$corpus_measured_at"
corpus_count measured_largest_issue_documents "$corpus_measured_documents" "$corpus_measured_at"
corpus_count measured_largest_issue_comments "$corpus_measured_comments" "$corpus_measured_at"
corpus_count measured_largest_issue_messages "$corpus_measured_messages" "$corpus_measured_at"
corpus_count measured_largest_issue_asks "$corpus_measured_asks" "$corpus_measured_at"
corpus_read_largest
baseline_start
# The bearer is the one the earlier release's server accepts: the run's DISPATCH_TOKEN, or the one
# baseline_start made. It is printed nowhere, and no child process inherits it.
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="seed-corpus-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The seeded corpus of the issue tree"

section=shapes
# The shapes issue's spec, version 1: the memo, the summary, four requirements, a decision, an ask
# block and the shapes.
{
  printf ':::callout{#memo kind="note" title=""}\nThe corpus holds every shape the tree census reads. An earlier release writes all of it. This head reads it after the upgrade.\n:::\n\n'
  printf 'The summary paragraph of the corpus'"'"'s main issue.\n\n## Requirements\n\n'
  printf ':::requirement{#r-shapes}\nEvery shape the census reads is in the corpus.\n:::\n\n'
  printf ':::requirement{#r-versions}\nEach document holds several versions.\n:::\n\n'
  printf ':::requirement{#r-approvals}\nSome approvals still stand after the upgrade.\n:::\n\n'
  printf ':::requirement{#r-citations}\nComments and documents cite blocks.\n:::\n\n'
  printf '## Decisions\n\n:::decision{#d-postgres}\nThe corpus lives in the pod'"'"'s own test database.\n:::\n\n'
  printf '## Questions\n\n:::ask{#q-second-image urgency="low"}\nShould the corpus hold a second image?\n:::\n\n'
  corpus_shapes spec shapes-spec
} >"$work/shapes-spec.md"
corpus_issue "$section" "The shapes of the issue tree" "$work/shapes-spec.md"
shapes="$corpus_key"
shapes_spec="$corpus_spec"
readonly shapes shapes_spec
corpus_revise "$section" "$shapes_spec" spec 2
corpus_edit "$section" "write version 3 of spec" "$shapes_spec" "$(jq -cn \
  '[{op: "insert", after: "block:r-citations", markdown: ":::requirement{#r-results}\nResults are recorded on the checks.\n:::"}]')"
pass "$section" "$shapes's spec holds three versions"

# verification.md: versions 1 and 2, and a third uploaded under the same name with one more check.
verification_md() {
  printf 'How the corpus is proved.\n\n'
  printf ':::check{#c-shapes serves="dispatch://%s/spec#b-r-shapes" surface="api" run="bash e2e/acceptance/issue-tree-graph.sh" when="before_merge" negative_control="Leave a shape out and the census misses it."}\nThe census finds every shape.\n:::\n\n' "$shapes"
  printf ':::check{#c-versions serves="dispatch://%s/spec#b-r-versions" surface="cli" run="bash e2e/acceptance/version-render-boundary.sh" negative_control="Render one version from the tuples and its bytes differ."}\nEvery version renders the bytes it holds.\n:::\n\n' "$shapes"
  printf ':::check{#c-three serves="dispatch://%s/spec#b-r-approvals dispatch://%s/spec#b-r-citations dispatch://%s/spec#b-r-shapes" surface="api" run="bash e2e/acceptance/issue-tree-queries.sh" negative_control="Drop one serves target and the coverage reads a gap."}\nOne check serves three requirements.\n:::\n\n' "$shapes" "$shapes" "$shapes"
  corpus_shapes verification.md shapes-verification
}
verification_md >"$work/shapes-verification.md"
corpus_upload "$section" "$shapes" verification.md "$work/shapes-verification.md"
shapes_verification="$corpus_doc"
readonly shapes_verification
corpus_revise "$section" "$shapes_verification" verification.md 2
{
  verification_md | sed 's/^A paragraph under the heading of verification.md\.$/A paragraph under the heading of verification.md, as version 2 writes it./'
  printf ':::check{#c-results serves="dispatch://%s/spec#b-r-results" surface="cli" run="dispatch results" negative_control="Record no result and the check reads unproved."}\nEach result is recorded on its check.\n:::\n' "$shapes"
} >"$work/shapes-verification-3.md"
corpus_upload "$section" "$shapes" verification.md "$work/shapes-verification-3.md"
corpus_version "$section" "$shapes_verification"
[[ "$tree_value" == 3 ]] ||
  fail "$section" "the upload under verification.md writes its third version" "its newest version is $tree_value"
pass "$section" "$shapes's verification.md holds three versions"

# plan.md: three versions; the person approves the second, so the third supersedes that approval.
{
  printf 'How the corpus is built.\n\n'
  printf ':::plan_task{#t-seed serves="dispatch://%s/spec#b-r-shapes"}\nWrite the shapes through the earlier release.\n:::\n\n' "$shapes"
  printf ':::plan_task{#t-versions serves="dispatch://%s/spec#b-r-versions" needs="dispatch://%s/artifact/plan-md#b-t-seed"}\nWrite each document again.\n:::\n\n' "$shapes" "$shapes"
  printf ':::plan_task{#t-approve serves="dispatch://%s/spec#b-r-approvals" needs="dispatch://%s/artifact/plan-md#b-t-versions"}\nApprove some documents.\n:::\n\n' "$shapes" "$shapes"
  corpus_shapes plan.md shapes-plan
} >"$work/shapes-plan.md"
corpus_upload "$section" "$shapes" plan.md "$work/shapes-plan.md"
shapes_plan="$corpus_doc"
readonly shapes_plan
corpus_revise "$section" "$shapes_plan" plan.md 2

# notes.md: a document no role names, three versions, one of its blocks citing the spec.
{
  printf '# Notes\n\nThe corpus meets dispatch://%s/spec#b-r-shapes, as dispatch://%s/artifact/verification-md#b-c-shapes proves.\n\n' "$shapes" "$shapes"
  corpus_shapes notes.md shapes-notes
} >"$work/shapes-notes.md"
corpus_upload "$section" "$shapes" notes.md "$work/shapes-notes.md"
shapes_notes="$corpus_doc"
readonly shapes_notes
corpus_revise "$section" "$shapes_notes" notes.md 2
corpus_edit "$section" "write version 3 of notes.md" "$shapes_notes" "$(jq -cn \
  '[{op: "insert", after: "end", markdown: "A paragraph the third version of the notes adds."}]')"
pass "$section" "$shapes's notes.md holds three versions"

corpus_image "$section" "$shapes" diagram.png
shapes_image="$corpus_doc"
readonly shapes_image
pass "$section" "uploaded the image diagram.png ($shapes_image)"

# The approvals: the spec, verification.md and notes.md at their newest version, which stand, and
# plan.md at its second, which its third then supersedes.
corpus_wait_settled "$section"
corpus_review "$section" "$shapes_spec" approved
corpus_review "$section" "$shapes_verification" approved
corpus_review "$section" "$shapes_notes" approved
corpus_review "$section" "$shapes_plan" approved
corpus_revise "$section" "$shapes_plan" plan.md 3
unset 'corpus_approved[$shapes_plan]'
pass "$section" "$shapes's plan.md holds three versions, its approval of version 2 superseded"

# The conversation: comments citing a block of each document, a person's thread with an agent's
# reply to its second block and the person's reply, an agent's status comment, messages, two asks.
corpus_blocks "$section" "$shapes_notes"
notes_paragraph="$corpus_paragraph"
corpus_comment agent "$section" "$shapes" "Status: the corpus is written, issue by issue."
corpus_comment agent "$section" "$shapes" \
  "dispatch://$shapes/spec#b-r-shapes is proved by dispatch://$shapes/artifact/verification-md#b-c-shapes."
corpus_comment person "$section" "$shapes" \
  "Which shapes does dispatch://$shapes/artifact/plan-md#b-t-seed write?"$'\n\n'"The second block of the person's comment asks about the image."
thread="$corpus_item"
corpus_comment agent "$section" "$shapes" \
  "The image is dispatch://$shapes/artifact/diagram-png, and dispatch://$shapes/artifact/notes-md#b-$notes_paragraph says why." \
  "$thread" b2
corpus_comment person "$section" "$shapes" "Then the notes settle it." "$thread"
corpus_comment person "$section" "$shapes" "dispatch://$shapes/spec#b-d-postgres stands."
pass "$section" "wrote six comments on $shapes, each but the status citing a block"
corpus_message agent "$section" "$shapes" \
  "The versions are proved by dispatch://$shapes/artifact/verification-md#b-c-versions."
corpus_message person "$section" "$shapes" "Is every document approved?"
corpus_message agent "$section" "$shapes" "All but plan.md, whose approval its third version supersedes." "$corpus_item"
pass "$section" "wrote three messages on $shapes"
corpus_ask "$section" "$shapes" "Which store holds the corpus?"
corpus_answer "$section" "$corpus_item" "Postgres, the database the pod runs." Postgres
corpus_ask "$section" "$shapes" "Which store holds the next corpus?"
pass "$section" "opened two asks on $shapes and answered one"
corpus_result "$section" "$shapes" c-shapes pass "$corpus_commit_one" tester
corpus_result "$section" "$shapes" c-versions fail "$corpus_commit_one" implementer
corpus_result "$section" "$shapes" c-versions pass "$corpus_commit_two" implementer
corpus_result "$section" "$shapes" c-three pass "$corpus_commit_two" tester
pass "$section" "recorded four results on three checks of $shapes"

# The child issue, under the shapes issue: its check and its plan task serve its parent too. Each
# of its three documents holds the shapes and two versions.
{
  printf ':::requirement{#r-child}\nThe child holds a requirement of its own.\n:::\n\nThe child'"'"'s summary.\n\n'
  corpus_shapes spec child-spec
} >"$work/child-spec.md"
corpus_issue "$section" "A child of the shapes issue" "$work/child-spec.md" "$shapes"
child="$corpus_key"
child_spec="$corpus_spec"
readonly child child_spec
corpus_revise "$section" "$child_spec" spec 2
{
  printf ':::check{#c-child serves="dispatch://%s/spec#b-r-shapes dispatch://%s/spec#b-r-child" surface="cli" run="bash e2e/acceptance/issue-tree-child-view.sh" negative_control="Move the check and its parent reads a gap."}\nThe child proves its parent'"'"'s shapes and its own requirement.\n:::\n\n' \
    "$shapes" "$child"
  corpus_shapes verification.md child-verification
} >"$work/child-verification.md"
corpus_upload "$section" "$child" verification.md "$work/child-verification.md"
corpus_revise "$section" "$corpus_doc" verification.md 2
{
  printf ':::plan_task{#t-child serves="dispatch://%s/spec#b-r-versions"}\nThe child writes versions of its parent'"'"'s documents.\n:::\n\n' \
    "$shapes"
  corpus_shapes plan.md child-plan
} >"$work/child-plan.md"
corpus_upload "$section" "$child" plan.md "$work/child-plan.md"
corpus_revise "$section" "$corpus_doc" plan.md 2
corpus_result "$section" "$child" c-child pass "$corpus_commit_one" tester
corpus_comment person "$section" "$child" "The child serves dispatch://$shapes/spec#b-r-shapes."
corpus_wait_settled "$section"
corpus_review "$section" "$child_spec" approved
pass "$section" "wrote the child $child under $shapes"

section=boundary
# The issues R19's counts and the dry run read beside the others.
boundary_issue() {
  local title="$1" label="$2"
  printf ':::requirement{#r-%s}\nThe %s issue holds one requirement.\n:::\n\n' "$label" "$label" >"$work/boundary-spec.md"
  corpus_shapes spec "$label-spec" >>"$work/boundary-spec.md"
  corpus_issue "$section" "$title" "$work/boundary-spec.md"
  boundary_key="$corpus_key"
  boundary_spec="$corpus_spec"
  corpus_revise "$section" "$boundary_spec" spec 2
  {
    printf ':::check{#c-%s serves="dispatch://%s/spec#b-r-%s" surface="cli" run="bash e2e/acceptance/seed-corpus.sh" negative_control="Remove the requirement and the check serves nothing."}\nThe %s issue is proved.\n:::\n\n' \
      "$label" "$boundary_key" "$label" "$label"
    corpus_shapes verification.md "$label-verification"
  } >"$work/boundary-verification.md"
  corpus_upload "$section" "$boundary_key" verification.md "$work/boundary-verification.md"
  boundary_verification="$corpus_doc"
  corpus_revise "$section" "$boundary_verification" verification.md 2
  {
    printf ':::plan_task{#t-%s serves="dispatch://%s/spec#b-r-%s"}\nBuild the %s issue.\n:::\n\n' \
      "$label" "$boundary_key" "$label" "$label"
    corpus_shapes plan.md "$label-plan"
  } >"$work/boundary-plan.md"
  corpus_upload "$section" "$boundary_key" plan.md "$work/boundary-plan.md"
  boundary_plan="$corpus_doc"
  corpus_revise "$section" "$boundary_plan" plan.md 2
  corpus_wait_settled "$section"
}

boundary_issue "An issue approved at its newest versions" standing
standing="$boundary_key"
readonly standing
corpus_review "$section" "$boundary_spec" approved
corpus_review "$section" "$boundary_verification" approved
corpus_review "$section" "$boundary_plan" approved
pass "$section" "$standing's three documents are approved at their newest version"

boundary_issue "An issue whose approval a later version supersedes" superseded
superseded="$boundary_key"
readonly superseded
corpus_review "$section" "$boundary_spec" approved
corpus_edit "$section" "write version 3 of $superseded's spec" "$boundary_spec" "$(jq -cn \
  '[{op: "insert", after: "end", markdown: "A paragraph written after the approval."}]')"
unset 'corpus_approved[$boundary_spec]'
corpus_review "$section" "$boundary_verification" changes_requested "Say which version the check reads."
pass "$section" "$superseded's spec approval is superseded and its verification.md asked changes of"

boundary_issue "An issue approved and then asked changes of" changed
changed="$boundary_key"
readonly changed
corpus_review "$section" "$boundary_spec" approved
corpus_review "$section" "$boundary_spec" changes_requested "The requirement needs a second sentence."
corpus_review "$section" "$boundary_plan" approved
pass "$section" "$changed's spec reads changes requested after its approval"

boundary_issue "A closed issue the conversion passes over" closed
closed="$boundary_key"
readonly closed
corpus_review "$section" "$boundary_spec" approved
corpus_comment person "$section" "$closed" "Closing: dispatch://$closed/spec#b-r-closed is met."
tree_send PATCH "/api/v1/issues/$closed" "$(jq -cn --argjson a "$(tree_actor)" '{status: "done", actor: $a}')"
tree_want 200 "$section" "close $closed"
expect "$section" "$closed is done" '"done"' .status

printf 'An issue that holds a spec and no other document.\n\n:::requirement{#r-alone}\nThe spec stands alone.\n:::\n' >"$work/alone-spec.md"
corpus_issue "$section" "An issue of a spec alone, blocked by another" "$work/alone-spec.md" "" "$standing"
alone="$corpus_key"
readonly alone
corpus_message agent "$section" "$alone" "Waiting on $standing."
pass "$section" "wrote $alone, blocked by $standing"

section=large
corpus_large_issue "$section" "$large_blocks" "$large_documents" "$large_comments" "$large_messages" "$large_asks"
large="$corpus_key"
readonly large

section=largest
printf 'The earlier release assigns a key before the role documents are copied.\n' >"$work/largest-starter.md"
corpus_issue "$section" "Copy of the largest issue's role documents" "$work/largest-starter.md"
readonly largest_copy="$corpus_key"
dropped_pointers="$(corpus_rewrite_largest "$largest_copy")" ||
  fail "$section" "prepare the role documents for $largest_copy" "rewrite failed"
[[ "$dropped_pointers" =~ ^[0-9]+$ ]] ||
  fail "$section" "count removed pointer attributes" "the rewrite returned no count"
declare -Ar role_names=([spec]=spec [verification-md]=verification.md [plan-md]=plan.md)
for role in spec verification-md plan-md; do
  corpus_upload_largest_role "$largest_copy" "$role" "${role_names[$role]}" "$work/$role.bootstrap.md"
done
corpus_wait_settled "$section"
for role in spec verification-md plan-md; do
  if ! cmp -s "$work/$role.bootstrap.md" "$work/$role.copy.md"; then
    corpus_upload_largest_role "$largest_copy" "$role" "${role_names[$role]}" "$work/$role.copy.md"
  fi
done
corpus_wait_settled "$section"
printf 'largest_issue_copy=%s pointer_attributes_dropped=%s at %s (match this key to items= in the conversion dry run)\n' \
  "$largest_copy" "$dropped_pointers" "$(corpus_now)"
pass "$section" "wrote the three role documents of $largest_copy through the earlier release"

section=counts
corpus_wait_settled "$section"
corpus_approvals_stand "$section"
# What the earlier release stored, as the database reads it with its own clock: R19's two counts
# first, in the spec's own queries, then the rest of the corpus.
count_query() {
  query "$section" "count $1" "select clock_timestamp(), ($2)"
  local at value
  IFS='|' read -r at value <<<"$answer"
  [[ "$value" =~ ^[0-9]+$ ]] || fail "$section" "count $1" "read '$value'"
  corpus_count "$1" "$value" "$at"
  counted="$value"
}
count_query pre_cutover_versions "select count(*) from artifact_versions where markdown is not null"
((counted > 0)) || fail "$section" "the corpus holds versions with a markdown body" "it holds none"
pass "$section" "R19's first count reads $counted versions with a markdown body"
count_query standing_approvals "with newest as (
    select distinct on (artifact_id) artifact_id, state, version
    from artifact_reviews
    order by artifact_id, event_seq desc nulls last, created_at desc, id desc
  )
  select count(*) from newest n
  where n.state = 'approved'
    and n.version = (select max(number) from artifact_versions v where v.artifact_id = n.artifact_id)"
[[ "$counted" == "${#corpus_approved[@]}" ]] ||
  fail "$section" "R19's second count reads every approval that stands" \
    "it reads $counted, and ${#corpus_approved[@]} approvals the corpus wrote stand"
pass "$section" "R19's second count reads the $counted standing approvals the corpus wrote"
count_query versions_without_markdown "select count(*) from artifact_versions where markdown is null"
((counted > 0)) || fail "$section" "the corpus holds a version with no markdown" "it holds none"
pass "$section" "the image's version holds no markdown"
count_query issues "select count(*) from issues"
count_query documents "select count(*) from artifacts where kind = 'doc'"
count_query images "select count(*) from artifacts where kind = 'image'"
count_query versions "select count(*) from artifact_versions"
count_query reviews "select count(*) from artifact_reviews"
count_query comments "select count(*) from comments"
count_query messages "select count(*) from messages"
count_query asks "select count(*) from asks"
count_query results "select count(*) from check_results"
count_query issue_links "select count(*) from issue_links"
count_query large_issue_comments "select count(*) from comments where issue_key = '$large'"
[[ "$counted" == "$large_comments" ]] ||
  fail "$section" "the large issue holds $large_comments comments" "the database holds $counted"
count_query large_issue_messages "select count(*) from messages where issue_key = '$large'"
[[ "$counted" == "$large_messages" ]] ||
  fail "$section" "the large issue holds $large_messages messages" "the database holds $counted"
count_query large_issue_asks "select count(*) from asks where issue_key = '$large'"
[[ "$counted" == "$large_asks" ]] ||
  fail "$section" "the large issue holds $large_asks asks" "the database holds $counted"
count_query large_issue_documents "select count(*) from artifacts where issue_key = '$large' and kind = 'doc'"
[[ "$counted" == "$large_documents" ]] ||
  fail "$section" "the large issue holds $large_documents documents" "the database holds $counted"
count_query large_issue_document_blocks "select count(*) from document_blocks b
  join artifacts a on a.id = b.artifact_id where a.issue_key = '$large'"
[[ "$counted" == "$corpus_large_written" ]] ||
  fail "$section" "the large issue's document_blocks rows are the $corpus_large_written blocks its documents' blocks reads counted" \
    "the database holds $counted"
pass "$section" "the database holds the large issue's documents, blocks, comments, messages and asks as written"

# The earlier release's server goes and the database stays, for the measurements and the drivers
# that boot this head over it.
baseline_stop_server
printf 'seed-corpus.sh wrote project %s from %s to %s: shapes %s, child %s, boundary %s %s %s %s %s, large %s\n' \
  "$project" "$started" "$(corpus_now)" "$shapes" "$child" "$standing" "$superseded" \
  "$changed" "$closed" "$alone" "$large"
printf 'PASS: seed-corpus.sh complete 6 sections\n'
