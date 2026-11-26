# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-admin-tag.sh: the step "admin tag". Sourced by operator-token-minters.sh,
# whose header says what each step checks.
# --- admin tag ---
# The broker keeps a secret's tags as its Secret Manager annotations (cmd/cipher/secret_gcp.go, show:
# withTags(secret.GetAnnotations()); internal/cipher/policy/policy.go, TagAdmin "admin" and TagTier
# "tier"), under the secret <secrets_prefix><slug>, the slug being the name lowercased with each
# underscore a hyphen (policy.NameToSlug). So the step reads them with `gcloud secrets describe`,
# which needs secretmanager.secrets.get alone and no broker.
step="admin tag of $SECRET"
secret_slug="$(printf '%s' "$SECRET" | tr 'A-Z_' 'a-z-')"
secret_id="$SECRETS_PREFIX$secret_slug"
if [ -z "$SECRETS_PREFIX" ]; then
  unknown "$step" "no --secrets-prefix: where the broker keeps $SECRET is not named"
elif ! have gcloud; then
  unknown "$step" "gcloud is not on PATH"
elif gcloud secrets describe "$secret_id" --format=json --quiet >"$WORK/secret.json" 2>"$WORK/secret.err"; then
  if ! jq -e 'type == "object" and (.annotations // {} | type == "object")' "$WORK/secret.json" >/dev/null 2>&1; then
    unknown "$step" "gcloud secrets describe $secret_id printed no secret"
  else
    admin="$(jq -r '.annotations.admin // ""' "$WORK/secret.json")"
    tier="$(jq -r '.annotations.tier // ""' "$WORK/secret.json")"
    [ "$admin" = true ] || finding "secret:$SECRET" "admin is \"$admin\", not \"true\": without the administrator tag a model judge may decide its release" \
      admin "secret:$SECRET" "" ""
    [ "$tier" = human ] || finding "secret:$SECRET" "tier is \"$tier\", not \"human\"" tier "secret:$SECRET" "" ""
    if [ "$admin" = true ] && [ "$tier" = human ]; then
      pass "$step" "admin is \"true\" and tier is \"human\": each release waits on a person's approval"
    fi
  fi
elif grep -q 'NOT_FOUND: Secret \[' "$WORK/secret.err"; then
  finding "secret:$SECRET" "no such secret: the operator's credential must be stored at human tier with --admin true" secret "secret:$SECRET" "" ""
else
  unknown "$step" "gcloud secrets describe $secret_id was refused (it needs secretmanager.secrets.get on the secret): $(first_line "$WORK/secret.err")"
fi
