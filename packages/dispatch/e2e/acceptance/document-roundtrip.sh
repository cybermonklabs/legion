#!/usr/bin/env bash
# Exercises the project-document API against a running native Dispatch server.
set -euo pipefail

readonly project="${PROJECT:-CORE}"
readonly name="${NAME:-Runbook.md}"
readonly user="${DISPATCH_USER:-acceptance}"

require() {
  local variable="$1"
  if [[ -z "${!variable:-}" ]]; then
    printf 'required environment variable %s is unset\n' "$variable" >&2
    exit 2
  fi
}

for command in curl jq; do
  command -v "$command" >/dev/null || {
    printf 'required command %s is unavailable\n' "$command" >&2
    exit 2
  }
done
require DISPATCH_URL

readonly headers=(
  -H 'Content-Type: application/json'
  -H "X-Dispatch-User: $user"
)

project_status="$(
  curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --request POST "${headers[@]}" \
    --data "$(jq --null-input --arg key "$project" --arg name "$project documents" '{key: $key, name: $name}')" \
    "$DISPATCH_URL/api/v1/projects"
)"
case "$project_status" in
  201 | 409) ;;
  *)
    printf 'create project %s: expected 201 or 409, got %s\n' "$project" "$project_status" >&2
    exit 1
    ;;
esac

work="$(mktemp -d)"
readonly work
trap 'rm -rf "$work"' EXIT

# A run after the first finds the project and the document its last run left, and Dispatch refuses
# a replacement that names no token, so the upload names the token of the version it replaces where
# the project holds that document; a first run finds none and sends none. Every read of a document
# goes by its slug: $name is the caller's to set and may carry a space or a `#`, which a path would
# have to encode and a slug never carries, and an unencoded name would be read as a document the
# project does not hold, letting the upload report the 400 PRECONDITION_REQUIRED that follows as
# the cause. The listing says which documents the project holds, so a name it does not list is that
# first run, and any other answer stops the script where it came.
curl --fail-with-body --silent --show-error --output "$work/documents" "${headers[@]}" \
  "$DISPATCH_URL/api/v1/projects/$project/artifacts"
held_slug="$(jq --raw-output --arg name "$name" \
  'map(select(.issue_key == null and (.name == $name or .slug == $name))) | .[0].slug // empty' \
  <"$work/documents")"
readonly held_slug
if [[ -n "$held_slug" ]]; then
  curl --fail-with-body --silent --show-error --output "$work/text" "${headers[@]}" \
    "$DISPATCH_URL/api/v1/projects/$project/artifacts/$held_slug/text"
  base="$(jq --exit-status --raw-output '.token' <"$work/text")"
else
  base=""
fi
readonly base
artifact="$(
  curl --fail-with-body --silent --show-error \
    --request POST "${headers[@]}" \
    --data "$(jq --null-input --arg name "$name" --arg content $'# Runbook\n\nStep one.\n' --arg base "$base" \
      'if $base == "" then {name: $name, content: $content} else {name: $name, content: $content, precondition: {document: $base}} end')" \
    "$DISPATCH_URL/api/v1/projects/$project/artifacts"
)"
readonly artifact
readonly artifact_id="$(jq --exit-status --raw-output '.artifact.id' <<<"$artifact")"
readonly slug="$(jq --exit-status --raw-output '.artifact.slug' <<<"$artifact")"

comment="$(
  curl --fail-with-body --silent --show-error \
    --request POST "${headers[@]}" \
    --data "$(jq --null-input --arg body "Review dispatch://$project/artifact/$slug" '{body: $body}')" \
    "$DISPATCH_URL/api/v1/artifacts/$artifact_id/comments"
)"
readonly comment
readonly comment_id="$(jq --exit-status --raw-output '.id' <<<"$comment")"

references="$(
  curl --fail-with-body --silent --show-error "${headers[@]}" \
    "$DISPATCH_URL/api/v1/artifacts/$artifact_id/references"
)"
readonly references
jq --exit-status --arg comment_id "$comment_id" \
  '.referenced_by[] | select(.kind == "comment" and .id == $comment_id)' \
  <<<"$references" >/dev/null

printf 'document roundtrip passed: %s/%s created; comment %s appears in references\n' \
  "$project" "$slug" "$comment_id"
