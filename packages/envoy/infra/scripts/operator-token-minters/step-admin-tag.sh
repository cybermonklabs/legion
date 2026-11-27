# shellcheck shell=bash
# shellcheck disable=SC2034,SC2153,SC2154 # globals set and read across the sourced files, in operator-token-minters.sh's order
# operator-token-minters/step-admin-tag.sh: the step "admin tag". Sourced by operator-token-minters.sh,
# whose header says what each step checks.
# --- admin tag ---
# The broker keeps a secret's tags as its Secret Manager annotations (cmd/cipher/secret_gcp.go, show:
# withTags(secret.GetAnnotations()); internal/cipher/policy/policy.go, TagAdmin "admin" and TagTier
# "tier"), under the secret <secrets_prefix><slug>, the slug being the name lowercased with each
# underscore a hyphen (policy.NameToSlug). So the step reads them with `gcloud secrets describe`,
# which needs secretmanager.secrets.get, and no broker.
#
# A release route is standing whatever its tags. Without the administrator tag or at agent tier a
# model judge may decide a release; with both, each release waits on one person's approval, the
# person an agent acts for included, and on no second person's: INFRA-840's c-operator-grant-door
# says "An agent's request that is approved by the person the agent acts for has one person in it,
# not two, so it does not meet this rule either." Nobody holds the operator identity standing
# (INFRA-840's R16), so each is a finding, the last named an open route until R16's grant door exists.
# What passes is no such secret, and only once the step has seen that --secrets-prefix names where
# the broker keeps its secrets: a mistyped prefix in the right project answers NOT_FOUND too, for a
# secret that is there. So on NOT_FOUND it lists the prefix's own project (`gcloud secrets list`,
# which needs secretmanager.secrets.list there, every page: no --limit) and passes only when a secret
# of that prefix is listed; none is UNKNOWN, since a place holding none cannot be told from a wrong
# one. Each listed name's last segment is compared with the prefix's, whichever form gcloud prints.
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
      finding "secret:$SECRET" "admin is \"true\" and tier is \"human\": each release waits on one person's approval and no second person's, so whoever may ask for it holds the operator identity standing (INFRA-840's R16), an open route until R16's grant door exists" \
        release "secret:$SECRET" "" ""
    fi
  fi
elif grep -q 'NOT_FOUND: Secret \[' "$WORK/secret.err"; then
  prefix_project="${SECRETS_PREFIX#projects/}" prefix_id="${SECRETS_PREFIX##*/}"
  prefix_project="${prefix_project%%/*}"
  if ! gcloud secrets list --project="$prefix_project" --format=json --quiet >"$WORK/secrets.json" 2>"$WORK/secrets.err"; then
    unknown "$step" "no such secret $secret_id, and gcloud secrets list --project=$prefix_project was refused (it needs secretmanager.secrets.list on the project), so whether --secrets-prefix names where the broker keeps its secrets is unknown: $(first_line "$WORK/secrets.err")"
  elif ! listed="$(jq -r --arg p "$prefix_id" 'if type == "array" then [.[] | .name? | strings | split("/") | last | select(startswith($p))] | length else error("no list") end' \
    "$WORK/secrets.json" 2>"$WORK/secrets.err")"; then
    unknown "$step" "no such secret $secret_id, and gcloud secrets list --project=$prefix_project printed no list of secrets: $(first_line "$WORK/secrets.err")"
  elif [ "$listed" -eq 0 ]; then
    unknown "$step" "no such secret $secret_id, and project $prefix_project lists no secret whose id starts $prefix_id: either the broker keeps no secret there or --secrets-prefix names the wrong place, which cannot be told apart, so the absence is not read as no release route"
  else
    pass "$step" "no such secret $secret_id, and project $prefix_project lists $listed secret(s) whose id starts $prefix_id, so --secrets-prefix names where the broker keeps its secrets: nothing releases the operator's credential through the broker"
  fi
else
  unknown "$step" "gcloud secrets describe $secret_id was refused (it needs secretmanager.secrets.get on the secret): $(first_line "$WORK/secret.err")"
fi
