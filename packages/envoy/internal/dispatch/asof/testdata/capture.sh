#!/usr/bin/env bash
# Captures, from a Dispatch deployment's event log (GET /api/v1/events/log), the payload shapes
# that the shadows of package asof read, into this directory:
#
#   capture.sh INVENTORY
#     reads INVENTORY, the TSV of payload shapes that ../../testdata/event-log-capture.sh
#     describes (shape_rows). Of the rows of the types a shadow reads a payload of (shadow_types
#     below), it fetches each shape's first event and writes it, redacted, as
#     payloads/<type>-<first_id>.json. It writes every row of INVENTORY, of every type, under its
#     header, as payload_shapes.tsv. TestEachShadowReadsEveryPayloadShapeProductionHolds reads
#     both: a row of a type a shadow reads with no fixture fails it, and so does a fixture that no
#     row names. The directory payloads/ is replaced whole.
#
# The request, its token and the redaction are ../../testdata/event-log-capture.sh's, which
# textversions/testdata/capture.sh uses too; its header says what they keep and what they
# replace.
set -euo pipefail

# The event types whose payload a shadow reads (the event types that asof.Shadows names).
shadow_types="issue.created issue.updated issue.closed issue.claimed issue.released ask.opened ask.answered ask.edited ask.resolved ask.handed_back ask.asked_of ask.anchor_refreshed ask.follower_added ask.follower_removed comment.created comment.answered comment.edited comment.deleted comment.resolved comment.reopened comment.anchor_refreshed suggestion.accepted suggestion.rejected comment.delivery message.created message.answered message.edited message.deleted message.delivery artifact.renamed project.created project.updated settings.project_default_assignee.updated settings.repo_project.updated"

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=../../testdata/event-log-capture.sh
source -- "$here/../../testdata/event-log-capture.sh"

capture_shapes() {
  local inventory=$1 out=$work/payloads shapes=$work/shapes.tsv type first_id keys name
  shape_rows "$inventory" "$shadow_types" >"$shapes"
  [[ -s $shapes ]] || {
    echo "capture.sh: the inventory names no shape of the types the shadows read" >&2
    exit 1
  }
  mkdir -- "$out"
  while IFS=$'\t' read -r type first_id keys <&3; do
    name=$type-$first_id
    capture_first_event "$type" "$first_id" "$keys" "$name" "$out/$name.json"
  done 3<"$shapes"
  {
    printf 'type\tfirst_id\tfirst_at\tevents\tpayload_keys\n'
    awk -F '\t' '$2 ~ /^[0-9]+$/' "$inventory" | LC_ALL=C sort -t $'\t' -k1,1 -k2,2n
  } >"$work/payload_shapes.tsv"
  rm -rf -- "$here/payloads"
  mv -- "$out" "$here/payloads"
  mv -- "$work/payload_shapes.tsv" "$here/payload_shapes.tsv"
  echo "capture.sh: wrote $(wc -l <"$shapes") shapes of $(cut -f1 "$shapes" | sort -u | wc -l) types into payloads/" >&2
}

main() {
  case $# in
  1) capture_shapes "$1" ;;
  *)
    echo "usage: capture.sh INVENTORY" >&2
    exit 2
    ;;
  esac
}

# One line, so bash has read all of it before main runs: an edit of this file while a capture
# runs cannot change what the run does next.
main "$@"; exit
