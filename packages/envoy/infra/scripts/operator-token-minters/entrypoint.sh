#!/usr/bin/env bash
# The minter-auditor image entrypoint. It accepts no argv: the approved Job pins every input below as
# an environment variable (packages/envoy/infra/README.md, "Minter-auditor render inputs"). It builds
# a gcloud context for each of the four clusters as the pod's Google service account, fetches the
# five render objects, each at the generation whose commit and run id it read, runs the minter check
# on them, and prints the check's report with those objects added. Any input or call it cannot
# complete is UNKNOWN, exit 2, and the check does not run.
set -euo pipefail

unknown() { printf 'UNKNOWN entrypoint: %s\n' "$*" >&2; exit 2; }
[ "$#" -eq 0 ] || unknown "takes no argument"
command -v jq >/dev/null || unknown "jq is not on PATH"

scalars=(
  MINTERS_AUDITOR_RENDERS_BUCKET MINTERS_AUDITOR_RENDERS_ADMIN MINTERS_AUDITOR_PROJECT MINTERS_AUDITOR_ANALYSIS_SCOPE
  MINTERS_AUDITOR_NAMESPACE MINTERS_AUDITOR_SERVICE_ACCOUNT MINTERS_AUDITOR_GSA
  MINTERS_AUDITOR_BROKER_ACCOUNT MINTERS_AUDITOR_BROKER_WORKLOAD MINTERS_AUDITOR_SQL_INSTANCE
  MINTERS_AUDITOR_DB_SECRET MINTERS_AUDITOR_BROKER_STATE MINTERS_AUDITOR_SIGNING_SECRET
  MINTERS_AUDITOR_SECRET MINTERS_AUDITOR_POOL_PROJECT MINTERS_AUDITOR_FLEET
  MINTERS_AUDITOR_OPERATOR_CLUSTER MINTERS_AUDITOR_OPERATOR_LOCATION MINTERS_AUDITOR_REPORT
)
for name in "${scalars[@]}"; do
  value="${!name:-}"
  [ -n "$value" ] || unknown "$name is unset or empty"
done

arrays=(MINTERS_AUDITOR_CLUSTERS MINTERS_AUDITOR_AGENT_NAMESPACES MINTERS_AUDITOR_GUARDED_PAIRS MINTERS_AUDITOR_RBAC_ONLY_MEMBERS MINTERS_AUDITOR_SERVICE_AGENTS MINTERS_AUDITOR_RENDER_WRITERS)
for name in "${arrays[@]}"; do
  value="${!name:-}"
  [ -n "$value" ] || unknown "$name is unset or empty"
  jq -e 'type == "array"' <<<"$value" >/dev/null 2>&1 || unknown "$name is not a JSON array"
done

# One entry for each GKE cluster of the pool; the check finds any the list leaves out
# (--audited-cluster, "live: audited clusters").
jq -e '
  length > 0
  and all(.[]; type == "object" and ((keys | sort) == ["location", "name", "project"])
      and all(.project, .location, .name; type == "string" and length > 0 and (contains("/") | not)))
  and ([.[] | [.project, .location, .name] | @json] | unique | length) == length
' <<<"$MINTERS_AUDITOR_CLUSTERS" >/dev/null 2>&1 ||
  unknown "MINTERS_AUDITOR_CLUSTERS must be one or more unique {project,location,name} objects"
for name in MINTERS_AUDITOR_AGENT_NAMESPACES MINTERS_AUDITOR_GUARDED_PAIRS MINTERS_AUDITOR_RBAC_ONLY_MEMBERS MINTERS_AUDITOR_SERVICE_AGENTS MINTERS_AUDITOR_RENDER_WRITERS; do
  jq -e 'all(.[]; type == "string" and length > 0)' <<<"${!name}" >/dev/null 2>&1 ||
    unknown "$name must contain nonempty strings"
done
jq -e 'length > 0' <<<"$MINTERS_AUDITOR_AGENT_NAMESPACES" >/dev/null || unknown "MINTERS_AUDITOR_AGENT_NAMESPACES is empty"
jq -e 'length == 4' <<<"$MINTERS_AUDITOR_RENDER_WRITERS" >/dev/null || unknown "MINTERS_AUDITOR_RENDER_WRITERS must name four writers"
bucket="${MINTERS_AUDITOR_RENDERS_BUCKET%/}"
case "$bucket" in gs://*) ;; *) unknown "MINTERS_AUDITOR_RENDERS_BUCKET is not a gs:// URL" ;; esac
for tool in gcloud kubectl curl agent-secrets gke-gcloud-auth-plugin; do
  command -v "$tool" >/dev/null || unknown "$tool is not on PATH"
done

work="$(mktemp -d "${TMPDIR:-/tmp}/minters-auditor.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export USE_GKE_GCLOUD_AUTH_PLUGIN=True
kubeconfig="$work/kubeconfig"
for cluster in $(jq -c '.[]' <<<"$MINTERS_AUDITOR_CLUSTERS"); do
  project="$(jq -r .project <<<"$cluster")"
  location="$(jq -r .location <<<"$cluster")"
  name="$(jq -r .name <<<"$cluster")"
  KUBECONFIG="$kubeconfig" gcloud container clusters get-credentials "$name" --project="$project" --location="$location" --quiet \
    >/dev/null 2>"$work/context.err" || unknown "cannot build the gcloud context for cluster $project/$location/$name: $(head -n 1 "$work/context.err")"
done

# Each object's metadata is read from the API's own description (--raw: gcloud's default format
# renames the fields), and its bytes are fetched at the generation that description names, so the
# commit and run id reported are those of the bytes the check reads.
renders=()
for object in rbac/cluster-grants secrets/dispatch secrets/broker iam/bootstrap iam/root; do
  metadata="$work/${object//\//-}.metadata.json"
  render="$work/${object//\//-}.json"
  gcloud storage objects describe "$bucket/$object" --raw --format=json >"$metadata" 2>"$work/storage.err" ||
    unknown "cannot read render object $object: $(head -n 1 "$work/storage.err")"
  generation="$(jq -er '.generation | select(type == "string" and test("^[0-9]+$"))' "$metadata" 2>/dev/null)" ||
    unknown "render object $object has no generation"
  commit="$(jq -er '.metadata.commit | select(type == "string" and length > 0)' "$metadata" 2>/dev/null)" ||
    unknown "render object $object has no metadata commit"
  run_id="$(jq -er '.metadata.run_id | select(type == "string" and length > 0)' "$metadata" 2>/dev/null)" ||
    unknown "render object $object has no metadata run_id"
  gcloud storage cp "$bucket/$object#$generation" "$render" --quiet 2>"$work/storage.err" ||
    unknown "cannot fetch render object $object at generation $generation: $(head -n 1 "$work/storage.err")"
  renders+=("$(jq -nc --arg object "$object" --arg commit "$commit" --arg run_id "$run_id" --arg generation "$generation" \
    '{object: $object, commit: $commit, run_id: $run_id, generation: $generation}')")
done

args=(
  --project "$MINTERS_AUDITOR_PROJECT" --cluster "$MINTERS_AUDITOR_OPERATOR_CLUSTER" --location "$MINTERS_AUDITOR_OPERATOR_LOCATION"
  --pool-project "$MINTERS_AUDITOR_POOL_PROJECT" --namespace "$MINTERS_AUDITOR_NAMESPACE" --service-account "$MINTERS_AUDITOR_SERVICE_ACCOUNT"
  --gsa "$MINTERS_AUDITOR_GSA" --broker-account "$MINTERS_AUDITOR_BROKER_ACCOUNT" --broker-workload "$MINTERS_AUDITOR_BROKER_WORKLOAD"
  --sql-instance "$MINTERS_AUDITOR_SQL_INSTANCE" --db-secret "$MINTERS_AUDITOR_DB_SECRET" --broker-state "$MINTERS_AUDITOR_BROKER_STATE"
  --signing-secret "$MINTERS_AUDITOR_SIGNING_SECRET" --secret "$MINTERS_AUDITOR_SECRET" --analysis-scope "$MINTERS_AUDITOR_ANALYSIS_SCOPE"
  --fleet "$MINTERS_AUDITOR_FLEET" --renders-bucket "$bucket" --renders-admin "$MINTERS_AUDITOR_RENDERS_ADMIN"
  --rbac-render "$work/rbac-cluster-grants.json"
  --secret-render "$work/secrets-dispatch.json" --secret-render "$work/secrets-broker.json"
  --iam-preview "$work/iam-bootstrap.json" --iam-preview "$work/iam-root.json" --report "$work/report.json"
)
while IFS= read -r value; do args+=(--agent-namespace "$value"); done < <(jq -r '.[]' <<<"$MINTERS_AUDITOR_AGENT_NAMESPACES")
while IFS= read -r value; do args+=(--guarded "$value"); done < <(jq -r '.[]' <<<"$MINTERS_AUDITOR_GUARDED_PAIRS")
while IFS= read -r value; do args+=(--rbac-only-member "$value"); done < <(jq -r '.[]' <<<"$MINTERS_AUDITOR_RBAC_ONLY_MEMBERS")
while IFS= read -r value; do args+=(--service-agent "$value"); done < <(jq -r '.[]' <<<"$MINTERS_AUDITOR_SERVICE_AGENTS")
while IFS= read -r value; do args+=(--render-writer "$value"); done < <(jq -r '.[]' <<<"$MINTERS_AUDITOR_RENDER_WRITERS")
while IFS= read -r value; do args+=(--audited-cluster "$value"); done < <(jq -r '.[] | "\(.project)/\(.location)/\(.name)"' <<<"$MINTERS_AUDITOR_CLUSTERS")

set +e
operator-token-minters.sh "${args[@]}"
status=$?
set -e
[ -r "$work/report.json" ] || unknown "the minter check wrote no report"
jq --argjson renders "[$(IFS=,; echo "${renders[*]}")]" '. + {renders: $renders}' "$work/report.json" >"$MINTERS_AUDITOR_REPORT" ||
  unknown "cannot write MINTERS_AUDITOR_REPORT"
cat "$MINTERS_AUDITOR_REPORT"
exit "$status"
