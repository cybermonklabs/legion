#!/usr/bin/env bash
# Drives the document a name alone makes, end to end against a running server, as an agent sees
# it: `dispatch artifact --issue <KEY> --name <title>` with neither --path nor --content makes an
# empty document and says it was made, not uploaded, naming the document's dispatch:// reference;
# that reference reads back one version whose markdown is empty; `dispatch read --issue` lists the
# document under `Documents:`; `dispatch doc-edit` writes a line into it and the line reads back;
# the same name with no content a second time is refused, naming the document that holds the
# title, and the document keeps what it held; a name that says anything other than Markdown
# (rows.jsonl) is refused by the Markdown-name rule before a request is sent, so the issue holds
# no such document; and a create with --path and one with --content each still make a document.
#
# Inputs: DISPATCH_URL (the server) and DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer,
# read here and never printed). It makes its own project (MAKEDOC, taken as it stands where the
# server already holds it) and one issue in that project, holding the documents the checks read;
# it closes that issue, also when a check fails. Nothing it writes is read by a second run, so it
# may be run again at once.
# Each passing check prints `PASS: <section> <what>` on stderr; the first failing one prints
# `FAIL: <section> <what>: <observed>` there and the script exits 1. Every diagnostic goes to
# stderr: the one line a run writes on stdout is its last, `all <n> assertions passed`, written
# only by a run that reached the end.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token create-document.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL
require_commands bun curl jq
read_bearer
# No child process inherits the bearer from here: read_bearer has unset it from the environment,
# curl reads it from a header file, and the CLI gets it in an environment this script builds.

readonly base="${DISPATCH_URL%/}"
# This driver's own project, so it needs no existing one; a server that already holds it keeps it.
readonly project="MAKEDOC"
script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
readonly cli_entry="$script_dir/../../../envoy-client/bin/dispatch.ts"
bun_bin="$(command -v bun)"
readonly bun_bin
readonly saved_path="$PATH"
readonly saved_tmpdir="${TMPDIR:-}"
session="create-document-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
closed=0
# How many assertions have passed: the number the last line reports.
count=0
cleanup() {
  local code=$?
  if [[ -n "$key" && "$closed" == 0 ]]; then
    cli issue-update --issue "$key" --status "done" \
      --reason "create-document.sh stopped before its last check; closing the issue it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'create-document.sh: could not close %s: %s\n' "$key" "$(<"$work/cleanup")" >&2
  fi
  rm -rf -- "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() {
  count=$((count + 1))
  printf 'PASS: %s %s\n' "$1" "$2" >&2
}
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$base"
    export DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}

# refused SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a zero exit fails,
# since every call here is one the CLI or the server must refuse.
refused() {
  local section="$1" what="$2"
  shift 2
  if cli "$@" >"$work/out" 2>&1; then
    fail "$section" "$what" "dispatch $1 took it: $(<"$work/out")"
  fi
}

q() { jq -rn --arg v "$1" '$v|@uri'; }

# send METHOD PATH [JSON]: sends METHOD PATH with the bearer in $work/auth. The answer's body goes
# to $work/body and its HTTP status to $status.
send() {
  local -a data=()
  [[ $# -lt 3 ]] || data=(-H 'Content-Type: application/json' --data-binary "$3")
  status="$(curl -sS -X "$1" -o "$work/body" -w '%{http_code}' -H "@$work/auth" "${data[@]}" \
    "$base$2" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# get PATH: send GET PATH.
get() { send GET "$1"; }
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# want_json SECTION WHAT WANT JQ-ARGS...: jq -cS JQ-ARGS over the last answer's body prints WANT.
# It counts no assertion of its own: each of the nine below passes once, at its end.
want_json() {
  local section="$1" what="$2" want="$3" got
  shift 3
  got="$(jq -cS "$@" "$work/body" 2>&1)" || fail "$section" "$what" "jq: $got; body: $(body)"
  [[ "$got" == "$want" ]] || fail "$section" "$what" "got $got, want $want"
}

# document_listed LINE: whether the last `dispatch read` listed LINE under `Documents:`. The
# section runs from that heading to the first line that is not one of its items.
document_listed() {
  local line listing=0
  while IFS= read -r line; do
    if [[ "$line" == "Documents:" ]]; then
      listing=1
      continue
    fi
    ((listing)) || continue
    [[ "$line" == "- "* ]] || return 1
    [[ "$line" != "$1" ]] || return 0
  done <"$work/out"
  return 1
}

# Setup: this driver's project, which a server that already holds it keeps, and one issue in it.
send POST "/api/v1/projects" "$(jq -cn --arg k "$project" --arg s "$session" \
  '{key: $k, name: "Documents made from a name", actor: {kind: "session", id: $s}}')"
case "$status" in
  201 | 409) ;;
  *) fail setup "create the project $project" "HTTP $status: $(body)" ;;
esac
printf 'A document made from its name alone is written in afterwards.\n' >"$work/spec.md"
run setup "create the issue" issue --project "$project" --title "Make a document from its name" \
  --spec-file "$work/spec.md" --force
[[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
  fail setup "create the issue" "no issue key in: $(<"$work/out")"
key="${BASH_REMATCH[1]}"
printf 'create-document.sh: writing in %s of %s\n' "$key" "$project" >&2

# 1: a create carrying a name and neither flag is taken.
run 1 "dispatch artifact --name \"Launch notes\" with neither flag" \
  artifact --issue "$key" --name "Launch notes"
pass 1 "dispatch artifact --issue $key --name \"Launch notes\" exits zero"

# 2: its line says the document was made, not uploaded, and carries its reference in parentheses.
[[ "$(<"$work/out")" =~ (dispatch://[^\);[:space:]]+)\) ]] ||
  fail 2 "the line carries the document's reference" "no reference in: $(<"$work/out")"
made_ref="${BASH_REMATCH[1]}"
[[ "$made_ref" == "dispatch://$key/artifact/launch-notes" ]] ||
  fail 2 "the reference addresses the new document" "got $made_ref"
grep -qF \
  "Made Launch notes as an empty document (artifact slug launch-notes; $made_ref). Write in it with dispatch doc-edit." \
  "$work/out" || fail 2 "the line says the document was made" "$(<"$work/out")"
if grep -qF 'Uploaded Launch notes' "$work/out"; then
  fail 2 "the line says made, not uploaded" "$(<"$work/out")"
fi
pass 2 "the line says Launch notes was made as an empty document, naming $made_ref"

# 3: the reference reads back one version, and that version's markdown is empty.
get "/api/v1/references?to=$(q "$made_ref")"
want_status 200 3 "read $made_ref"
doc_id="$(jq -er '.node.id' "$work/body")" || fail 3 "read $made_ref" "no node id: $(body)"
get "/api/v1/artifacts/$doc_id"
want_status 200 3 "read the document"
want_json 3 "the document is a doc holding one version" '["Launch notes","doc",1]' \
  '[.name, .kind, (.versions | length)]'
get "/api/v1/artifacts/$doc_id/text"
want_status 200 3 "read the document's text"
want_json 3 "the document's markdown is empty" '""' '.markdown'
pass 3 "$made_ref reads back one version whose markdown is empty"

# 4: the issue read lists the document by slug, filename and kind.
run 4 "dispatch read --issue $key" read --issue "$key"
document_listed "- launch-notes (Launch notes, doc)" ||
  fail 4 "the read lists the document under Documents:" "$(<"$work/out")"
pass 4 "dispatch read --issue $key lists launch-notes under Documents:"

# 5: an edit writes a line into the empty document.
run 5 "dispatch doc-edit into the empty document" doc-edit --issue "$key" --artifact launch-notes \
  --ops-json '[{"op":"insert","after":"end","markdown":"The first line."}]'
[[ "$(<"$work/out")" =~ Applied\ [0-9]+\ ops\ \(version\ ([0-9]+)\) ]] ||
  fail 5 "the edit reports the version it wrote" "no version in: $(<"$work/out")"
written_version="${BASH_REMATCH[1]}"
pass 5 "dispatch doc-edit wrote a line into launch-notes (version $written_version)"

# 6: the line reads back.
get "/api/v1/issues/$key/artifacts/launch-notes/text"
want_status 200 6 "read launch-notes after the edit"
want_json 6 "the edited document holds the line" '"The first line."' '.markdown | rtrimstr("\n")'
pass 6 "the line written into launch-notes reads back"

# 7: the same name with no content a second time is refused, naming the document that holds the
# title, and that document keeps the line. The command sends it: a create with no content has no
# version to write, so there is nothing --force could ask for and nothing for the command to
# refuse ahead of the server. The sentence is the server's own, which names the document the
# title finds, its slug, and what a write with no content does.
refused 7 "a second create under the name Launch notes" artifact --issue "$key" --name "Launch notes"
grep -qF 'names artifact launch-notes (Launch notes), which exists' "$work/out" ||
  fail 7 "the refusal names the document holding the title" "$(<"$work/out")"
get "/api/v1/issues/$key/artifacts/launch-notes/text"
want_status 200 7 "read launch-notes after the refusal"
want_json 7 "the refused create wrote nothing" '"The first line."' '.markdown | rtrimstr("\n")'
pass 7 "a second create under Launch notes is refused, naming the document that holds the title"

# 7b: force is what asks for a document's next version, and a create with no content has no
# version to write, so the server refuses it under a held name whatever force says. This is the
# sentence the merge of #3530 and this change left in dispatch_artifact's own description, and
# nothing else drives it: the document keeps the line afterwards.
refused 7b "a forced second create under the name Launch notes" artifact --issue "$key" --name "Launch notes" --force
grep -qF 'a write with no content makes no document and writes no version' "$work/out" ||
  fail 7b "the forced refusal names the create's own rule" "$(<"$work/out")"
get "/api/v1/issues/$key/artifacts/launch-notes/text"
want_status 200 7b "read launch-notes after the forced refusal"
want_json 7b "the forced create wrote nothing" '"The first line."' '.markdown | rtrimstr("\n")'
pass 7b "force does not empty a document: a content-free create under a held name is refused with it too"

# 8: a name that says anything other than Markdown is refused by the Markdown-name rule, here,
# before a request is sent: the issue holds no such document afterwards.
refused 8 "a create named rows.jsonl with no content" artifact --issue "$key" --name rows.jsonl
grep -qF '"rows.jsonl" is not a Markdown name' "$work/out" ||
  fail 8 "the refusal names the Markdown-name rule" "$(<"$work/out")"
get "/api/v1/issues/$key/artifacts/rows-jsonl"
want_status 404 8 "read rows.jsonl after the refusal"
pass 8 "--name rows.jsonl with no content is refused by the Markdown-name rule, sending nothing"

# 9: the two creates that carry a source still make a document, each at version 1.
printf '# From a file\n\nA line stored from a file.\n' >"$work/from-file.md"
run 9 "dispatch artifact --path" artifact --issue "$key" --name "from-file.md" \
  --path "$work/from-file.md"
grep -qF 'Uploaded from-file.md as version 1 (artifact slug from-file-md;' "$work/out" ||
  fail 9 "the file create reports its version" "$(<"$work/out")"
get "/api/v1/issues/$key/artifacts/from-file-md/text"
want_status 200 9 "read from-file.md"
want_json 9 "the file's text is stored" '"# From a file\n\nA line stored from a file."' \
  '.markdown | rtrimstr("\n")'
run 9 "dispatch artifact --content" artifact --issue "$key" --name "inline.md" \
  --content '# Inline

A line stored from the command line.
'
grep -qF 'Uploaded inline.md as version 1 (artifact slug inline-md;' "$work/out" ||
  fail 9 "the content create reports its version" "$(<"$work/out")"
get "/api/v1/issues/$key/artifacts/inline-md/text"
want_status 200 9 "read inline.md"
want_json 9 "the inline content is stored" '"# Inline\n\nA line stored from the command line."' \
  '.markdown | rtrimstr("\n")'
pass 9 "--path and --content each still make a document"

run close "close $key" issue-update --issue "$key" --status "done" \
  --reason "create-document.sh passed every check against this issue."
closed=1
printf 'create-document.sh: %s is done\n' "$key" >&2

printf 'all %d assertions passed\n' "$count"
